.PHONY: all build run test ytdlp windows clean

all: build

build:
	@echo "Building GrabAll daemon..."
	@CGO_ENABLED=0 go build -ldflags="-s -w" -o graball ./daemon
	@echo "Done! Binary at ./graball"

windows:
	@echo "Building GrabAll Native Win32 GUI for Windows (amd64)..."
	@GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -H=windowsgui" -o graball.exe ./daemon
	@echo "Done! Windows GUI executable at ./graball.exe"

run: build
	@./graball

test:
	@go test -v ./daemon/...

ytdlp:
	@./scripts/setup_ytdlp.sh

clean:
	@rm -f graball graball.exe
