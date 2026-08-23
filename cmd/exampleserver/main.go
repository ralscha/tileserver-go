// Command exampleserver serves the standalone MapLibre example during local development.
package main

import (
	"log"
	"net/http"
	"time"
)

const (
	address   = ":3000"
	directory = "./examples/maplibre"
)

func main() {
	server := &http.Server{
		Addr:              address,
		Handler:           http.FileServer(http.Dir(directory)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("serving %s at http://localhost%s", directory, address)
	log.Fatal(server.ListenAndServe())
}
