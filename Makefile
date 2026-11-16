.PHONY: build vet run demo drill
build:
	go build ./...
vet:
	go vet ./...
run:
	go run ./cmd/atlas
demo:
	python3 scripts/walkthrough.py --workflow submit-render --out var/demo
drill:
	python3 scripts/walkthrough.py --workflow retention-drill
