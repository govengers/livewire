module github.com/govengers/livewire/mcpadapter

go 1.26.0

// The adapter builds against the livewire of this checkout; a release requires the livewire
// version that ships the toolset (D82).
replace github.com/govengers/livewire => ../

require (
	github.com/google/uuid v1.6.0
	github.com/govengers/auth v1.3.0
	github.com/govengers/livewire v1.0.1
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/stretchr/testify v1.12.1
	go.uber.org/goleak v1.3.0
)

require (
	github.com/gabriel-vasile/mimetype v1.4.16 // indirect
	github.com/go-playground/locales v0.14.2 // indirect
	github.com/go-playground/universal-translator v0.18.2 // indirect
	github.com/go-playground/validator/v10 v10.30.5 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/leodido/go-urn v1.5.0 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
