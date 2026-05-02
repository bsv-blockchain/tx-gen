package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		log.Fatal("ADMIN_TOKEN env var required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	apiKey := os.Getenv("ARCADE_API_KEY")

	// BROADCAST_MODE: "callback" (default) or "sse"
	mode := os.Getenv("BROADCAST_MODE")
	if mode == "" {
		mode = "callback"
	}

	var callbackURL string
	if mode == "callback" {
		callbackURL = os.Getenv("CALLBACK_URL")
		if callbackURL == "" {
			log.Fatal("CALLBACK_URL env var required in callback mode (e.g. http://my-ip:8080)")
		}
	}

	woc := newWOCClient()
	lockScript := buildLockScript()
	scriptHash := scriptToHash(lockScript)
	log.Printf("scripthash: %s", scriptHash)
	log.Printf("broadcast mode: %s", mode)

	log.Println("fetching UTXOs...")
	utxos, err := woc.fetchUnspent(scriptHash)
	if err != nil {
		log.Fatalf("fetch UTXOs: %v", err)
	}
	log.Printf("loaded %d UTXOs", len(utxos))

	q := newQueue()
	for _, u := range utxos {
		q.Push(u)
	}

	arcade := newArcadeClient(apiKey, callbackURL)
	engine := newEngine(q, arcade, lockScript)
	server := newServer(engine, adminToken)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go engine.run(ctx)

	if mode == "sse" {
		go subscribeSSE(ctx, apiKey)
	}

	go func() {
		if err := server.start(":" + port); err != nil {
			log.Printf("server: %v", err)
			cancel()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutdown")
}
