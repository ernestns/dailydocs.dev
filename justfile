set shell := ["sh", "-cu"]

go_cache := ".cache/go-build"
go_bin := `sh -c 'gobin="$(go env GOBIN 2>/dev/null)"; if [ -n "$gobin" ]; then printf "%s" "$gobin"; else printf "%s/bin" "$(go env GOPATH)"; fi'`

default:
	@just --list

# Install local developer tooling
init:
	command -v go >/dev/null || { echo "missing go: brew install go" >&2; exit 1; }
	command -v brew >/dev/null || { echo "missing Homebrew: install shellcheck and gitleaks manually" >&2; exit 1; }
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	brew list shellcheck >/dev/null 2>&1 || brew install shellcheck
	brew list gitleaks >/dev/null 2>&1 || brew install gitleaks
	PATH="{{go_bin}}:$PATH" command -v staticcheck >/dev/null
	PATH="{{go_bin}}:$PATH" command -v govulncheck >/dev/null

# Run the Go test suite
test:
	mkdir -p {{go_cache}}
	GOCACHE="$PWD/{{go_cache}}" go test ./...

# Run tests and static analysis
check: test static-check

# Run static analysis and secret checks
static-check:
	mkdir -p {{go_cache}}
	PATH="{{go_bin}}:$PATH" command -v staticcheck >/dev/null || { echo "missing staticcheck: just init" >&2; exit 1; }
	PATH="{{go_bin}}:$PATH" command -v govulncheck >/dev/null || { echo "missing govulncheck: just init" >&2; exit 1; }
	command -v shellcheck >/dev/null || { echo "missing shellcheck: brew install shellcheck" >&2; exit 1; }
	command -v gitleaks >/dev/null || { echo "missing gitleaks: brew install gitleaks" >&2; exit 1; }
	GOCACHE="$PWD/{{go_cache}}" go vet ./...
	PATH="{{go_bin}}:$PATH" GOCACHE="$PWD/{{go_cache}}" staticcheck ./...
	PATH="{{go_bin}}:$PATH" GOCACHE="$PWD/{{go_cache}}" govulncheck ./...
	shellcheck bootstrap.sh scripts/*.sh
	gitleaks git --redact .

# Build ./bin/dailydocs
build:
	GOCACHE="$PWD/{{go_cache}}" ./scripts/build.sh

# Run checks required before deployment
pre-deploy: check build

# Build and run the web app locally
run: build
	./scripts/with-env.sh ./bin/dailydocs

# Check local / and /health endpoints
smoke:
	curl --fail http://localhost:8080/
	curl --fail http://localhost:8080/health

# Deploy using REMOTE and REPO_DIR from local .env
deploy: pre-deploy
	./scripts/with-env.sh sh -c 'test -n "$REMOTE" && test -n "$REPO_DIR" && REMOTE="$REMOTE" REPO_DIR="$REPO_DIR" ./scripts/deploy-remote.sh'
