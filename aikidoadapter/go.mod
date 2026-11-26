module github.com/govengers/livewire/aikidoadapter

go 1.26.0

// The adapter builds against the livewire of this checkout; a release requires the livewire
// version that ships the toolset (D85).
replace github.com/govengers/livewire => ../

require (
	github.com/govengers/aikido v1.2.0
	github.com/govengers/auth v1.3.0
	github.com/govengers/livewire v1.0.1
	github.com/stretchr/testify v1.12.1
	go.uber.org/goleak v1.3.0
)

require (
	github.com/gabriel-vasile/mimetype v1.4.16 // indirect
	github.com/go-playground/locales v0.14.2 // indirect
	github.com/go-playground/universal-translator v0.18.2 // indirect
	github.com/go-playground/validator/v10 v10.30.5 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674 // indirect
	github.com/leodido/go-urn v1.5.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)
