package gateway

import (
	"context"
	"log"
	"time"
)

func (s *Server) activateExtensions(ctx context.Context) func() {
	if s.extensions == nil {
		return func() {}
	}
	owner, err := s.extensions.BeginGatewayActivation(ctx)
	if err != nil {
		log.Printf("extension gateway activation: %v", err)
		return func() {}
	}
	return func() {
		drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := owner.Close(drain); err != nil {
			log.Printf("extension gateway activation shutdown: %v", err)
		}
	}
}
