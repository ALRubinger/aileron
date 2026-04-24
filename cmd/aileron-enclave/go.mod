module github.com/ALRubinger/aileron/cmd/aileron-enclave

go 1.25.0

require github.com/ALRubinger/aileron/internal v0.0.0

require (
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace github.com/ALRubinger/aileron/internal => ../../internal
