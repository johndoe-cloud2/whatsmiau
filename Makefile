.PHONY: dev run install

install:
	go mod download

run:
	go run main.go
