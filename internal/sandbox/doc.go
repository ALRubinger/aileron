// This file holds the go:generate directive for the WASM test fixture.
// Run `go generate ./internal/sandbox/...` to rebuild
// internal/sandbox/testdata/echo.wasm from
// internal/sandbox/testdata/echo/main.go. Requires Go 1.21+.

//go:generate sh -c "cd testdata/echo && GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags='-s -w' -o ../echo.wasm ."

package sandbox
