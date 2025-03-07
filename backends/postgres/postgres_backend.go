package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/acaloiaro/neoq"
	"github.com/acaloiaro/neoq/handler"
	"github.com/acaloiaro/neoq/internal"
	"github.com/acaloiaro/neoq/jobs"
	"github.com/acaloiaro/neoq/logging"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // nolint: revive
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/iancoleman/strcase"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jsuar/go-cron-descriptor/pkg/crondescriptor"
	"github.com/robfig/cron"
	"golang.org/x/exp/slog"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	PendingJobIDQuery = `SELECT id
					FROM neoq_jobs
					WHERE queue = $1
					AND status NOT IN ('processed')
					AND run_after <= NOW()
					FOR UPDATE SKIP LOCKED
					LIMIT 1`
	PendingJobQuery = `SELECT id,fingerprint,queue,status,deadline,payload,retries,max_retries,run_after,ran_at,created_at,error
					FROM neoq_jobs
					WHERE id = $1
					AND status NOT IN ('processed')
					AND run_after <= NOW()
					FOR UPDATE SKIP LOCKED
					LIMIT 1`
	FutureJobQuery = `SELECT id,run_after
					FROM neoq_jobs
					WHERE queue = $1
					AND status NOT IN ('processed')
					AND run_after > NOW()
					ORDER BY run_after ASC
					LIMIT 100
					FOR UPDATE SKIP LOCKED`
	setIdleInTxSessionTimeout = `SET idle_in_transaction_session_timeout = 0`
)

type contextKey struct{}

var (
	txCtxVarKey                   contextKey
	shutdownJobID                 = "-1" // job ID announced when triggering a shutdown
	shutdownAnnouncementAllowance = 100  // ms
	ErrCnxString                  = errors.New("invalid connecton string: see documentation for valid connection strings")
	ErrDuplicateJob               = errors.New("duplicate job")
	ErrNoTransactionInContext     = errors.New("context does not have a Tx set")
)

// PgBackend is a Postgres-based Neoq backend
type PgBackend struct {
	neoq.Neoq
	config      *neoq.Config
	logger      logging.Logger
	cron        *cron.Cron
	mu          *sync.RWMutex // mutex to protect mutating state on a pgWorker
	pool        *pgxpool.Pool
	futureJobs  map[string]time.Time       // map of future job IDs to their due time
	handlers    map[string]handler.Handler // a map of queue names to queue handlers
	cancelFuncs []context.CancelFunc       // A collection of cancel functions to be called upon Shutdown()
}

// Backend initializes a new postgres-backed neoq backend
//
// If the database does not yet exist, Neoq will attempt to create the database and related tables by default.
//
// Backend requires that one of the [neoq.ConfigOption] is [WithConnectionString]
//
// Connection strings may be a URL or DSN-style connection strings. The connection string supports multiple
// options detailed below.
//
// options:
//   - pool_max_conns: integer greater than 0
//   - pool_min_conns: integer 0 or greater
//   - pool_max_conn_lifetime: duration string
//   - pool_max_conn_idle_time: duration string
//   - pool_health_check_period: duration string
//   - pool_max_conn_lifetime_jitter: duration string
//
// # Example DSN
//
// user=worker password=secret host=workerdb.example.com port=5432 dbname=mydb sslmode=verify-ca pool_max_conns=10
//
// # Example URL
//
// postgres://worker:secret@workerdb.example.com:5432/mydb?sslmode=verify-ca&pool_max_conns=10
func Backend(ctx context.Context, opts ...neoq.ConfigOption) (pb neoq.Neoq, err error) {
	cfg := neoq.NewConfig()
	cfg.IdleTransactionTimeout = neoq.DefaultIdleTxTimeout

	p := &PgBackend{
		mu:          &sync.RWMutex{},
		config:      cfg,
		handlers:    make(map[string]handler.Handler),
		futureJobs:  make(map[string]time.Time),
		cron:        cron.New(),
		cancelFuncs: []context.CancelFunc{},
	}

	// Set all options
	for _, opt := range opts {
		opt(p.config)
	}

	p.logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: p.config.LogLevel}))
	ctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancelFuncs = append(p.cancelFuncs, cancel)
	p.mu.Unlock()

	err = p.initializeDB()
	if err != nil {
		return
	}

	if p.pool == nil {
		var poolConfig *pgxpool.Config
		poolConfig, err = pgxpool.ParseConfig(p.config.ConnectionString)
		if err != nil || p.config.ConnectionString == "" {
			return nil, ErrCnxString
		}

		// ensure that workers don't consume connections with idle transactions
		poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) (err error) {
			var query string
			if p.config.IdleTransactionTimeout > 0 {
				query = fmt.Sprintf("SET idle_in_transaction_session_timeout = '%dms'", p.config.IdleTransactionTimeout)
			} else {
				// there is no limit to the amount of time a worker's transactions may be idle
				query = setIdleInTxSessionTimeout
			}
			_, err = conn.Exec(ctx, query)
			return
		}

		p.pool, err = pgxpool.NewWithConfig(ctx, poolConfig)
		if err != nil {
			return
		}
	}

	p.cron.Start()

	pb = p

	return pb, nil
}

// WithConnectionString configures neoq postgres backend to use the specified connection string when connecting to a backend
func WithConnectionString(connectionString string) neoq.ConfigOption {
	return func(c *neoq.Config) {
		c.ConnectionString = connectionString
	}
}

// WithTransactionTimeout sets the time that PgBackend's transactions may be idle before its underlying connection is
// closed
// The timeout is the number of milliseconds that a transaction may sit idle before postgres terminates the
// transaction's underlying connection. The timeout should be longer than your longest job takes to complete. If set
// too short, job state will become unpredictable, e.g. retry counts may become incorrect.
func WithTransactionTimeout(txTimeout int) neoq.ConfigOption {
	return func(c *neoq.Config) {
		c.IdleTransactionTimeout = txTimeout
	}
}

// txFromContext gets the transaction from a context, if the transaction is already set
func txFromContext(ctx context.Context) (t pgx.Tx, err error) {
	var ok bool
	if t, ok = ctx.Value(txCtxVarKey).(pgx.Tx); ok {
		return
	}

	err = ErrNoTransactionInContext

	return
}

// initializeDB initializes the tables, types, and indices necessary to operate Neoq
//
//nolint:funlen,gocyclo,cyclop
func (p *PgBackend) initializeDB() (err error) {
	migrations, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		p.logger.Error("unable to run migrations", "error", err)
		return
	}

	// `pgx` supports config params that `pq` does not. Since pgx is neoq's primary SQL interface, user often configure
	// it with pgx-specific config params like `max_conn_count`. However, `go-migrate` uses `pq` under the hood, and
	// these `pgx` config params cause `pq` to throw an "unknown config parameter" error when they're encountered.
	// So we must first sanitize connection strings for pq
	var pgxCfg *pgx.ConnConfig
	pgxCfg, err = pgx.ParseConfig(p.config.ConnectionString)
	if err != nil {
		p.logger.Error("unable to run migrations", "error", err)
		return
	}

	sslMode := "verify-ca"
	// nil TLSConfig means "sslmode=disable" was set on the connection
	if pgxCfg.TLSConfig == nil {
		sslMode = "disable"
	}

	pqConnectionString := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s&x-migrations-table=neoq_schema_migrations",
		pgxCfg.User,
		pgxCfg.Password,
		pgxCfg.Host,
		pgxCfg.Database,
		sslMode)
	m, err := migrate.NewWithSourceInstance("iofs", migrations, pqConnectionString)
	if err != nil {
		p.logger.Error("unable to run migrations", "error", err)
		return
	}

	err = m.Up()
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		p.logger.Error("unable to run migrations", "error", err)
		return
	}

	return nil
}

// Enqueue adds jobs to the specified queue
func (p *PgBackend) Enqueue(ctx context.Context, job *jobs.Job) (jobID string, err error) {
	if job.Queue == "" {
		err = jobs.ErrNoQueueSpecified
		return
	}

	p.logger.Debug("enqueueing job payload", slog.Any("job_payload", job.Payload))

	p.logger.Debug("acquiring new connection from connection pool")
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		err = fmt.Errorf("error acquiring connection: %w", err)
		return
	}
	defer conn.Release()

	p.logger.Debug("beginning new transaction to enqueue job")
	tx, err := conn.Begin(ctx)
	if err != nil {
		err = fmt.Errorf("error creating transaction: %w", err)
		return
	}

	// Rollback is safe to call even if the tx is already closed, so if
	// the tx commits successfully, this is a no-op
	defer func(ctx context.Context) { _ = tx.Rollback(ctx) }(ctx) // rollback has no effect if the transaction has been committed

	// Make sure RunAfter is set to a non-zero value if not provided by the caller
	// if already set, schedule the future job
	now := time.Now().UTC()
	if job.RunAfter.IsZero() {
		p.logger.Debug("RunAfter not set, job will run immediately after being enqueued")
		job.RunAfter = now
	}

	jobID, err = p.enqueueJob(ctx, tx, job)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == pgerrcode.UniqueViolation {
				err = ErrDuplicateJob
				return
			}
		}
		p.logger.Error("error enqueueing job", "error", err)
		err = fmt.Errorf("error enqueuing job: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		err = fmt.Errorf("error committing transaction: %w", err)
		return
	}
	p.logger.Debug("job added to queue:", "job_id", jobID)

	// notify listeners that a new job has arrived if it's not a future job
	if job.RunAfter.Equal(now) {
		p.announceJob(ctx, job.Queue, jobID)
	} else {
		p.mu.Lock()
		p.futureJobs[jobID] = job.RunAfter
		p.mu.Unlock()
		p.logger.Debug("added job to future jobs list", "job_id", jobID, "run_after", job.RunAfter)
	}

	return jobID, nil
}

// Start starts processing jobs with the specified queue and handler
func (p *PgBackend) Start(ctx context.Context, h handler.Handler) (err error) {
	ctx, cancel := context.WithCancel(ctx)

	p.logger.Debug("starting job processing", "queue", h.Queue)
	p.mu.Lock()
	p.cancelFuncs = append(p.cancelFuncs, cancel)
	p.handlers[h.Queue] = h
	p.mu.Unlock()

	err = p.start(ctx, h)
	if err != nil {
		p.logger.Error("unable to start processing queue", "queue", h.Queue, "error", err)
		return
	}
	return
}

// StartCron starts processing jobs with the specified cron schedule and handler
//
// See: https://pkg.go.dev/github.com/robfig/cron?#hdr-CRON_Expression_Format for details on the cron spec format
func (p *PgBackend) StartCron(ctx context.Context, cronSpec string, h handler.Handler) (err error) {
	cd, err := crondescriptor.NewCronDescriptor(cronSpec)
	if err != nil {
		p.logger.Error("error creating cron descriptor", "cronspec", cronSpec, "error", err)
		return fmt.Errorf("error creating cron descriptor: %w", err)
	}

	cdStr, err := cd.GetDescription(crondescriptor.Full)
	if err != nil {
		p.logger.Error("error getting cron descriptor", "descriptor", crondescriptor.Full, "error", err)
		return fmt.Errorf("error getting cron description: %w", err)
	}

	queue := internal.StripNonAlphanum(strcase.ToSnake(*cdStr))
	h.Queue = queue

	ctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancelFuncs = append(p.cancelFuncs, cancel)
	p.mu.Unlock()

	if err = p.cron.AddFunc(cronSpec, func() {
		_, err := p.Enqueue(ctx, &jobs.Job{Queue: queue})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}

			p.logger.Error("error queueing cron job", "error", err)
		}
	}); err != nil {
		return fmt.Errorf("error adding cron: %w", err)
	}

	return p.Start(ctx, h)
}

// SetLogger sets this backend's logger
func (p *PgBackend) SetLogger(logger logging.Logger) {
	p.logger = logger
}

// Shutdown shuts this backend down
func (p *PgBackend) Shutdown(ctx context.Context) {
	p.logger.Debug("starting shutdown.")
	for queue := range p.handlers {
		p.announceJob(ctx, queue, shutdownJobID)
	}

	// wait for the announcement to process
	time.Sleep(time.Duration(shutdownAnnouncementAllowance) * time.Millisecond)

	for _, f := range p.cancelFuncs {
		f()
	}

	p.pool.Close()
	p.cron.Stop()

	p.cancelFuncs = nil
	p.logger.Debug("shutdown complete")
}

// enqueueJob adds jobs to the queue, returning the job ID
//
// Jobs that are not already fingerprinted are fingerprinted before being added
// Duplicate jobs are not added to the queue. Any two unprocessed jobs with the same fingerprint are duplicates
func (p *PgBackend) enqueueJob(ctx context.Context, tx pgx.Tx, j *jobs.Job) (jobID string, err error) {
	err = jobs.FingerprintJob(j)
	if err != nil {
		return
	}

	p.logger.Debug("adding job to the queue")
	err = tx.QueryRow(ctx, `INSERT INTO neoq_jobs(queue, fingerprint, payload, run_after, deadline)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		j.Queue, j.Fingerprint, j.Payload, j.RunAfter, j.Deadline).Scan(&jobID)
	if err != nil {
		err = fmt.Errorf("unable add job to queue: %w", err)
		return
	}

	return jobID, err
}

// moveToDeadQueue moves jobs from the pending queue to the dead queue
func (p *PgBackend) moveToDeadQueue(ctx context.Context, tx pgx.Tx, j *jobs.Job, jobErr error) (err error) {
	_, err = tx.Exec(ctx, "DELETE FROM neoq_jobs WHERE id = $1", j.ID)
	if err != nil {
		return
	}

	_, err = tx.Exec(ctx, `INSERT INTO neoq_dead_jobs(id, queue, fingerprint, payload, retries, max_retries, error, deadline)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		j.ID, j.Queue, j.Fingerprint, j.Payload, j.Retries, j.MaxRetries, jobErr.Error(), j.Deadline)

	return
}

// updateJob updates the status of jobs with: status, run time, error messages, and retries
//
// if the retry count exceeds the maximum number of retries for the job, move the job to the dead jobs queue
//
// if `tx`'s underlying connection dies while updating job status, the transaction will fail, and the job's original
// status will be reflecting in the database.
//
// The implication of this is that:
// - the job's 'error' field will not reflect any errors the occurred in the handler
// - the job's retry count is not incremented
// - the job's run time will remain its original value
// - the job has its original 'status'
//
// ultimately, this means that any time a database connection is lost while updating job status, then the job will be
// processed at least one more time.
// nolint: cyclop
func (p *PgBackend) updateJob(ctx context.Context, jobErr error) (err error) {
	status := internal.JobStatusProcessed
	errMsg := ""

	if jobErr != nil {
		p.logger.Error("job failed", "job_error", jobErr)
		status = internal.JobStatusFailed
		errMsg = jobErr.Error()
	}

	var job *jobs.Job
	if job, err = jobs.FromContext(ctx); err != nil {
		return fmt.Errorf("error getting job from context: %w", err)
	}

	var tx pgx.Tx
	if tx, err = txFromContext(ctx); err != nil {
		return fmt.Errorf("error getting tx from context: %w", err)
	}

	if job.Retries >= job.MaxRetries {
		err = p.moveToDeadQueue(ctx, tx, job, jobErr)
		return
	}

	var runAfter time.Time
	if status == internal.JobStatusFailed {
		runAfter = internal.CalculateBackoff(job.Retries)
		qstr := "UPDATE neoq_jobs SET ran_at = $1, error = $2, status = $3, retries = $4, run_after = $5 WHERE id = $6"
		_, err = tx.Exec(ctx, qstr, time.Now().UTC(), errMsg, status, job.Retries, runAfter, job.ID)
	} else {
		qstr := "UPDATE neoq_jobs SET ran_at = $1, error = $2, status = $3 WHERE id = $4"
		_, err = tx.Exec(ctx, qstr, time.Now().UTC(), errMsg, status, job.ID)
	}

	if err != nil {
		return
	}

	if time.Until(runAfter) > 0 {
		p.mu.Lock()
		p.futureJobs[fmt.Sprint(job.ID)] = runAfter
		p.mu.Unlock()
	}

	return nil
}

// start starts processing new, pending, and future jobs
// nolint: cyclop
func (p *PgBackend) start(ctx context.Context, h handler.Handler) (err error) {
	var ok bool

	if h, ok = p.handlers[h.Queue]; !ok {
		return fmt.Errorf("%w: %s", handler.ErrNoHandlerForQueue, h.Queue)
	}

	listenJobChan, ready := p.listen(ctx, h.Queue) // listen for 'new' jobs
	defer close(ready)

	pendingJobsChan := p.pendingJobs(ctx, h.Queue) // process overdue jobs *at startup*

	// wait for the listener to connect and be ready to listen
	<-ready

	// process all future jobs and retries
	go func() { p.scheduleFutureJobs(ctx, h.Queue) }()

	for i := 0; i < h.Concurrency; i++ {
		go func() {
			var err error
			var jobID string

			for {
				select {
				case jobID = <-listenJobChan:
					err = p.handleJob(ctx, jobID, h)
				case jobID = <-pendingJobsChan:
					err = p.handleJob(ctx, jobID, h)
				case <-ctx.Done():
					return
				}

				if err != nil {
					if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, context.Canceled) {
						err = nil
						continue
					}

					p.logger.Error("job failed", "error", err, "job_id", jobID)

					continue
				}
			}
		}()
	}

	return nil
}

// initFutureJobs is intended to be run once to initialize the list of future jobs that must be monitored for
// execution. it should be run only during system startup.
func (p *PgBackend) initFutureJobs(ctx context.Context, queue string) (err error) {
	rows, err := p.pool.Query(ctx, FutureJobQuery, queue)
	if err != nil {
		p.logger.Error("failed to fetch future jobs list", err)
		return
	}

	var id string
	var runAfter time.Time
	_, err = pgx.ForEachRow(rows, []any{&id, &runAfter}, func() error {
		p.mu.Lock()
		p.futureJobs[id] = runAfter
		p.mu.Unlock()
		return nil
	})

	return
}

// scheduleFutureJobs announces future jobs using NOTIFY on an interval
func (p *PgBackend) scheduleFutureJobs(ctx context.Context, queue string) {
	err := p.initFutureJobs(ctx, queue)
	if err != nil {
		return
	}

	// check for new future jobs on an interval
	ticker := time.NewTicker(p.config.JobCheckInterval)

	for {
		// loop over list of future jobs, scheduling goroutines to wait for jobs that are due within the next 30 seconds
		p.mu.Lock()
		for jobID, runAfter := range p.futureJobs {
			timeUntillRunAfter := time.Until(runAfter)
			if timeUntillRunAfter <= p.config.FutureJobWindow {
				delete(p.futureJobs, jobID)
				go func(jid string) {
					scheduleCh := time.After(timeUntillRunAfter)
					<-scheduleCh
					p.announceJob(ctx, queue, jid)
				}(jobID)
			}
		}
		p.mu.Unlock()

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return
		}
	}
}

// announceJob announces jobs to queue listeners.
//
// Announced jobs are executed by the first worker to respond to the announcement.
func (p *PgBackend) announceJob(ctx context.Context, queue, jobID string) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return
	}

	// Rollback is safe to call even if the tx is already closed, so if
	// the tx commits successfully, this is a no-op
	defer func(ctx context.Context) { _ = tx.Rollback(ctx) }(ctx)

	// notify listeners that a job is ready to run
	_, err = tx.Exec(ctx, fmt.Sprintf("NOTIFY %s, '%s'", queue, jobID))
	if err != nil {
		return
	}

	err = tx.Commit(ctx)
	if err != nil {
		return
	}
}

func (p *PgBackend) pendingJobs(ctx context.Context, queue string) (jobsCh chan string) {
	jobsCh = make(chan string)

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		p.logger.Error("failed to acquire database connection to listen for pending queue items", err)
		return
	}

	go func(ctx context.Context) {
		defer conn.Release()

		for {
			jobID, err := p.getPendingJobID(ctx, conn, queue)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, context.Canceled) {
					break
				}

				p.logger.Error("failed to fetch pending job", "error", err, "job_id", jobID)
			} else {
				jobsCh <- jobID
			}
		}
	}(ctx)

	return
}

// handleJob is the workhorse of Neoq
// it receives pending, periodic, and retry job ids asynchronously
// 1. handleJob first creates a transactions inside of which a row lock is acquired for the job to be processed.
// 2. handleJob secondly calls the handler on the job, and finally updates the job's status
func (p *PgBackend) handleJob(ctx context.Context, jobID string, h handler.Handler) (err error) {
	var job *jobs.Job
	var tx pgx.Tx
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()

	tx, err = conn.Begin(ctx)
	if err != nil {
		return
	}
	defer func(ctx context.Context) { _ = tx.Rollback(ctx) }(ctx) // rollback has no effect if the transaction has been committed

	job, err = p.getPendingJob(ctx, tx, jobID)
	if err != nil {
		return
	}

	if job.Deadline != nil && job.Deadline.Before(time.Now().UTC()) {
		err = jobs.ErrJobExceededDeadline
		p.logger.Debug("job deadline is in he past, skipping", "job_id", job.ID)
		err = p.updateJob(ctx, err)
		return
	}

	ctx = withJobContext(ctx, job)
	ctx = context.WithValue(ctx, txCtxVarKey, tx)

	// check if the job is being retried and increment retry count accordingly
	if job.Status != internal.JobStatusNew {
		job.Retries++
	}

	// execute the queue handler of this job
	jobErr := handler.Exec(ctx, h)
	err = p.updateJob(ctx, jobErr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}

		err = fmt.Errorf("error updating job status: %w", err)
		return err
	}

	err = tx.Commit(ctx)
	if err != nil {
		errMsg := "unable to commit job transaction. retrying this job may dupliate work:"
		p.logger.Error(errMsg, "error", err, "job_id", job.ID)
		return fmt.Errorf("%s %w", errMsg, err)
	}

	return nil
}

// listen uses Postgres LISTEN to listen for jobs on a queue
// TODO: There is currently no handling of listener disconnects in PgBackend.
// This will lead to jobs not getting processed until the worker is restarted.
// Implement disconnect handling.
func (p *PgBackend) listen(ctx context.Context, queue string) (c chan string, ready chan bool) {
	c = make(chan string, p.handlers[queue].Concurrency)
	ready = make(chan bool)

	go func(ctx context.Context) {
		conn, err := p.pool.Acquire(ctx)
		if err != nil {
			p.logger.Error("unable to acquire new listener connnection", "error", err)
			return
		}
		defer p.release(ctx, conn, queue)

		// set this connection's idle in transaction timeout to infinite so it is not intermittently disconnected
		_, err = conn.Exec(ctx, fmt.Sprintf("SET idle_in_transaction_session_timeout = '0'; LISTEN %s", queue))
		if err != nil {
			err = fmt.Errorf("unable to configure listener connection: %w", err)
			p.logger.Error("unable to configure listener connection", "error", err)
			return
		}

		// notify start() that we're ready to listen for jobs
		ready <- true

		for {
			notification, waitErr := conn.Conn().WaitForNotification(ctx)
			if waitErr != nil {
				if errors.Is(waitErr, context.Canceled) {
					return
				}

				p.logger.Error("failed to wait for notification", "error", waitErr)
				continue
			}

			// check if Shutdown() has been called
			if notification.Payload == shutdownJobID {
				return
			}

			c <- notification.Payload
		}
	}(ctx)

	return c, ready
}

func (p *PgBackend) release(ctx context.Context, conn *pgxpool.Conn, queue string) {
	query := fmt.Sprintf("SET idle_in_transaction_session_timeout = '%d'; UNLISTEN %s", p.config.IdleTransactionTimeout, queue)
	_, err := conn.Exec(ctx, query)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}

		p.logger.Error("unable to reset connection config before release", err)
	}

	conn.Release()
}

func (p *PgBackend) getPendingJob(ctx context.Context, tx pgx.Tx, jobID string) (job *jobs.Job, err error) {
	row, err := tx.Query(ctx, PendingJobQuery, jobID)
	if err != nil {
		return
	}

	job, err = pgx.CollectOneRow(row, pgx.RowToAddrOfStructByName[jobs.Job])
	if err != nil {
		return
	}

	return
}

func (p *PgBackend) getPendingJobID(ctx context.Context, conn *pgxpool.Conn, queue string) (jobID string, err error) {
	err = conn.QueryRow(ctx, PendingJobIDQuery, queue).Scan(&jobID)
	return
}

// withJobContext creates a new context with the Job set
func withJobContext(ctx context.Context, j *jobs.Job) context.Context {
	return context.WithValue(ctx, internal.JobCtxVarKey, j)
}
