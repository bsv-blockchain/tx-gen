package main

import (
	"log"
	"runtime/debug"
	"sync"
)

func SafeGo(wg *sync.WaitGroup, name string, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in %s: %v\n%s", name, r, debug.Stack())
				IncPanic(name)
			}
		}()
		fn()
	}()
}
