package main

import (
	"context"
	"log"

	"github.com/acaloiaro/neoq"
)

func main() {
	var err error
	const queue = "foobar"
	//
	nq, _ := neoq.New("postgres://postgres:postgres@127.0.0.1:5432/neoq?sslmode=disable")

	handler := neoq.NewHandler(func(ctx context.Context) (err error) {
		var j *neoq.Job
		j, err = neoq.JobFromContext(ctx)
		log.Println("got job id:", j.ID, "messsage:", j.Payload["message"])
		return
	})

	err = nq.Listen(queue, handler)
	if err != nil {
		log.Println("error listening to queue", err)
	}

	// this code will exit quickly since since Listen() is asynchronous
	// real applications should call Listen() on startup for every queue that needs to be handled
}
