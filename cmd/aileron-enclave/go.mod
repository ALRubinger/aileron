module github.com/ALRubinger/aileron/cmd/aileron-enclave

go 1.24

require (
	github.com/ALRubinger/aileron/core v0.0.0
	github.com/ALRubinger/aileron/enclave v0.0.0
)

require (
	golang.org/x/crypto v0.36.0 // indirect
	golang.org/x/sys v0.31.0 // indirect
)

replace (
	github.com/ALRubinger/aileron/core => ../../core
	github.com/ALRubinger/aileron/enclave => ../../enclave
)
