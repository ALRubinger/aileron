module github.com/ALRubinger/aileron/cmd/aileron-connector-dev-run

go 1.25.0

require github.com/ALRubinger/aileron/internal v0.0.0

replace (
	github.com/ALRubinger/aileron/internal => ../../internal
	github.com/ALRubinger/aileron/sdk/go => ../../sdk/go
)
