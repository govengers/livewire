// Command todo-cli is the generated command-line client of the API (D68), configured by
// TODO_BASE_URL and TODO_API_KEY or the flags --base-url and --api-key.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/govengers/livewire/examples/todo/client/todocommand"
)

// snippet:begin cli-main

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := todocommand.Run(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// snippet:end cli-main
