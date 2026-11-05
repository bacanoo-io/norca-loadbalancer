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
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bacanoo-io/norca-loadbalancer/internal/config"
	_ "github.com/bacanoo-io/norca-loadbalancer/internal/providers/all"
	"github.com/bacanoo-io/norca-loadbalancer/internal/registrar"
	"github.com/bacanoo-io/norca-loadbalancer/internal/server"
	"github.com/bacanoo-io/norca-loadbalancer/pkg/provider"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "types" {
		printTypes(os.Stdout, provider.Default)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lbprovider:", err)
		os.Exit(1)
	}
}

func run(parent context.Context, args []string) error {
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
	slog.SetDefault(logger)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	built, err := cfg.Build(provider.Default)
	if err != nil {
		return err
	}
	defer built.Close()

	srvClasses := make([]*server.Class, len(built.Classes))
	regs := make([]registrar.Registration, len(built.Classes))
	for i, c := range built.Classes {
		srvClasses[i] = &server.Class{Instance: c.Instance, Token: c.Token}
		regs[i] = registrar.Registration{
			Class:         c.Class,
			Endpoint:      c.Endpoint,
			TokenVariable: c.TokenVariable,
			Internal:      c.Internal,
			Ports:         c.Ports,
			Parameters:    c.Parameters,
		}
	}

	ctx, cancelRun := context.WithCancel(parent)
	defer cancelRun()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(srvClasses, provider.Default.Types(), logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 2)
	go func() {
		logger.Info("provider API listening", "address", cfg.Listen, "classes", len(srvClasses), "tls", cfg.TLS != nil)
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

	var wg sync.WaitGroup
	if *register {
		store, err := registrar.NewAPIStore(nil)
		if err != nil {
			return fmt.Errorf("Norca API: %w", err)
		}
		reg := &registrar.Registrar{
			Store:            store,
			Registrations:    regs,
			Interval:         cfg.Interval(),
			Version:          version,
			DeregisterOnExit: *deregister,
			Logger:           logger,
		}
		wg.Go(func() {
			if err := reg.Run(ctx); err != nil {
				errc <- err
			}
		})
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	cancelRun()

	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srvErr := srv.Shutdown(shutdown)
	wg.Wait()

	if runErr != nil {
		return runErr
	}
	return srvErr
}

// printTypes lists the types with their parameters.
func printTypes(w io.Writer, reg *provider.Registry) {
	for _, t := range reg.Types() {
		fmt.Fprintf(w, "%s - %s\n", t.ID, t.Description)
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
			fmt.Fprintf(w, "    %-24s %-8s%s%s  %s\n", p.Name, p.Type, def, extra, p.Description)
		}
		fmt.Fprintln(w)
	}
}
