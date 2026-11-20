package memdistill

import (
	"context"
	"slices"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func Bind(auto func() string, complete Complete) sdk.MemoryDistillationService {
	return sdk.MemoryDistillationService{
		Status: distillationStatus,
		Configure: func(settings SettingsChange) (bool, error) {
			_, read, err := Configure(settings)
			return read, err
		},
		ReadNow: func(ctx context.Context) {
			read, err := prepareRead(ctx, auto, complete, Listed)
			if err == nil {
				go read()
			}
		},
		Stop: Stop,
		SetDream: func(on bool) error {
			_, err := SetDream(on)
			return err
		},
		DreamNow: func(ctx context.Context) {
			dream, err := prepareDream(ctx, auto, complete)
			if err == nil {
				go dream()
			}
		},
		Undo: func() error {
			_, err := Undo()
			return err
		},
		Keep: func(ctx context.Context, first, interval time.Duration) {
			Keep(ctx, first, interval, auto, complete)
		},
	}
}

func distillationStatus() (sdk.MemoryDistillationStatus, error) {
	reading, dreaming, doing, of := activeRun()
	state, err := Load()
	if err != nil {
		return sdk.MemoryDistillationStatus{}, err
	}
	state.Done = nil
	return sdk.MemoryDistillationStatus{
		MemoryDistillationState: state,
		Running:                 reading,
		Dreaming:                dreaming,
		Doing:                   doing,
		Of:                      of,
		Ranges:                  slices.Clone(Ranges),
	}, nil
}
