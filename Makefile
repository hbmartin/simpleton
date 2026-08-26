.PHONY: build test verify schemas

build:
	go build -o bin/simpleton ./cmd/simpleton

test:
	go test ./...
	npm --prefix packs/typescript test
	python3 -m unittest discover -s packs/python -p 'test_*.py'

schemas:
	python3 scripts/verify_schemas.py

verify: test schemas build
