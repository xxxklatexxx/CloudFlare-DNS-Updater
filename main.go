package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	config := flag.String("config", "/config/config.json", "configuration path")
	once := flag.Bool("run-once", false, "check records once then exit")
	dry := flag.Bool("dry-run", false, "discover and report without modifying Cloudflare or applied state")
	health := flag.Bool("healthcheck", false, "check local dashboard health and exit")
	flag.Parse()
	c, err := loadConfig(*config)
	if err != nil {
		log.Fatal(err)
	}
	if *health {
		addr := c.Listen
		if len(addr) > 0 && addr[0] == ':' {
			addr = "127.0.0.1" + addr
		}
		resp, e := newClient(5 * time.Second).Get("http://" + addr + "/healthz")
		if e != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	token, err := readSecret(c.TokenFile)
	if err != nil {
		log.Fatal(err)
	}
	u, err := newUpdater(c, token, *dry)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *once {
		if err = u.reconcile(ctx, false); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	password, err := readSecret(c.PasswordFile)
	if err != nil {
		log.Fatal(err)
	}
	if len(password) < 16 {
		log.Fatal("dashboard password must contain at least 16 characters")
	}
	h, err := dashboard(ctx, u, password)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: c.Listen, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan struct{})
	go func() { defer close(done); u.schedule(ctx) }()
	go func() {
		<-ctx.Done()
		stop, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = server.Shutdown(stop)
	}()
	log.Printf("dashboard listening on %s; interval=%s dry_run=%t", c.Listen, c.poll, *dry)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		cancel()
		log.Fatal(fmt.Errorf("dashboard server: %w", err))
	}
	<-done
	// Wait for a manual reconciliation to stop before process exit.
	u.run.Lock()
	u.run.Unlock()
}
