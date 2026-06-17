# ServConsole Makefile

.PHONY: build run test docker-build clean

build:
	go build -o servconsole.exe main.go

run: build
	./servconsole.exe --port=8083

test:
	go test -v ./...

docker-build:
	docker build -t servconsole:latest .

clean:
	@if exist servconsole.exe del /f /q servconsole.exe
