// Command synthchat writes a fictional two-person chat export for demos and
// load tests. It never reads real data.
//
//	go run ./cmd/synthchat -format whatsapp -n 100000 > /tmp/demo.txt
//	go run ./cmd/synthchat -format telegram -n 100000 > /tmp/demo.json
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kshitijv97/between-us/backend/internal/synth"
)

func main() {
	format := flag.String("format", "whatsapp", "whatsapp or telegram")
	n := flag.Int("n", 1000, "number of messages")
	seed := flag.Uint64("seed", 42, "random seed (same seed, same output)")
	flag.Parse()

	cfg := synth.DefaultConfig(*n)
	cfg.Seed = *seed
	var err error
	switch *format {
	case "whatsapp":
		err = synth.WriteWhatsApp(os.Stdout, cfg)
	case "telegram":
		err = synth.WriteTelegram(os.Stdout, cfg)
	default:
		err = fmt.Errorf("unknown -format %q", *format)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "synthchat:", err)
		os.Exit(1)
	}
}
