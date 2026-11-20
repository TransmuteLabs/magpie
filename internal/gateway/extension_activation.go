package gateway

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/yetone/magpie/internal/extension"
)

func (s *Server) activateExtensions(ctx context.Context) func() {
	if s.extensions == nil {
		return func() {}
	}
	return serveExtensionOwner(ctx, "gateway activation", s.extensions.BeginGatewayActivation)
}

func serveExtensionOwner[Owner interface{ Close(context.Context) error }](ctx context.Context, name string, begin func(context.Context) (Owner, error)) func() {
	lifetime, stop := context.WithCancel(ctx)
	finish := func(owner Owner) {
		drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := owner.Close(drain); err != nil {
			log.Printf("extension %s shutdown: %v", name, err)
		}
	}
	owner, err := begin(lifetime)
	if err == nil {
		return func() {
			stop()
			finish(owner)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stop()
		for err != nil {
			var previous *extension.ListenerOwnerDraining
			if !errors.As(err, &previous) {
				if lifetime.Err() == nil {
					log.Printf("extension %s: %v", name, err)
				}
				return
			}
			if previous.Wait(lifetime) != nil {
				return
			}
			owner, err = begin(lifetime)
		}
		<-lifetime.Done()
		finish(owner)
	}()
	return func() {
		stop()
		<-done
	}
}
