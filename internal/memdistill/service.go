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
			go ReadAll(ctx, auto(), complete)
		},
		Stop: Stop,
		SetDream: func(on bool) error {
			_, err := SetDream(on)
			return err
		},
		DreamNow: func(ctx context.Context) {
			go Dream(ctx, auto(), complete)
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
	state, err := Load()
	if err != nil {
		return sdk.MemoryDistillationStatus{}, err
	}
	state.Done = nil
	dreaming := Dreaming()
	view := sdk.MemoryDistillationStatus{
		MemoryDistillationState: state,
		Running:                 Running() && !dreaming,
		Dreaming:                dreaming,
		Ranges:                  slices.Clone(Ranges),
	}
	if view.Running {
		view.Doing, view.Of = Progress()
	}
	return view, nil
}
