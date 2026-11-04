// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command lbprovider serves load balancer classes to the LoadBalancer
// operator of Norca and registers them.
//
//	lbprovider -config classes.json     run
//	lbprovider types                    list the linked types and their parameters
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bacanoo-io/norca-loadbalancer/config"
	"github.com/bacanoo-io/norca-loadbalancer/provider"
	_ "github.com/bacanoo-io/norca-loadbalancer/providers/all"
	"github.com/bacanoo-io/norca-loadbalancer/registrar"
	"github.com/bacanoo-io/norca-loadbalancer/server"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "types" {
		printTypes(provider.Default)
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lbprovider:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("lbprovider", flag.ContinueOnError)
	path := fs.String("config", "classes.json", "configuration file")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn or error")
	register := fs.Bool("register", true, "register the classes in Norca (NORCA_ADDR, NORCA_TOKEN)")
	deregister := fs.Bool("deregister-on-exit", true, "remove the registrations on exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("log level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	built, err := cfg.Build(provider.Default)
	if err != nil {
		return err
	}
	defer built.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(built.Classes, provider.Default.Types(), logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 2)
	go func() {
		logger.Info("provider API listening", "address", cfg.Listen, "classes", len(built.Classes), "tls", cfg.TLS != nil)
		var err error
		if cfg.TLS != nil {
			err = srv.ListenAndServeTLS(cfg.TLS.Cert, cfg.TLS.Key)
		} else {
			err = srv.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	if *register {
		store, err := registrar.NewAPIStore(nil)
		if err != nil {
			return fmt.Errorf("Norca API: %w", err)
		}
		reg := &registrar.Registrar{
			Store:            store,
			Registrations:    built.Registrations,
			Interval:         cfg.Interval(),
			Version:          version,
			DeregisterOnExit: *deregister,
			Logger:           logger,
		}
		go func() {
			if err := reg.Run(ctx); err != nil {
				errc <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		stop()
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// printTypes lists the types with their parameters.
func printTypes(reg *provider.Registry) {
	for _, t := range reg.Types() {
		fmt.Printf("%s - %s\n", t.ID, t.Description)
		for _, p := range t.Params {
			var flags []string
			if p.Required {
				flags = append(flags, "required")
			}
			if p.Secret {
				flags = append(flags, "secret")
			}
			if p.Dynamic {
				flags = append(flags, "dynamic")
			}
			def := ""
			if p.Default != nil && p.Default != "" {
				def = fmt.Sprintf(" (default %v)", p.Default)
			}
			extra := ""
			if len(flags) > 0 {
				extra = " [" + strings.Join(flags, ", ") + "]"
			}
			fmt.Printf("    %-24s %-8s%s%s  %s\n", p.Name, p.Type, def, extra, p.Description)
		}
		fmt.Println()
	}
}
