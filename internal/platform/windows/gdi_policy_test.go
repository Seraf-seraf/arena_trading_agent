package winplatform

import (
	"strings"
	"testing"
)

func TestValidateGDICaptureSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		snapshot   WindowSnapshot
		wantErr    bool
		wantReason string
	}{
		{
			name: "active window",
			snapshot: WindowSnapshot{
				Active:     true,
				ClientArea: PixelRectangle{Width: 1920, Height: 1080},
			},
		},
		{
			name: "inactive window",
			snapshot: WindowSnapshot{
				ClientArea: PixelRectangle{Width: 1920, Height: 1080},
			},
			wantErr:    true,
			wantReason: "запрещён во избежание записи другого приложения",
		},
		{
			name: "minimized window",
			snapshot: WindowSnapshot{
				Minimized:  true,
				ClientArea: PixelRectangle{Width: 1920, Height: 1080},
			},
			wantErr:    true,
			wantReason: "свёрнутое окно игры",
		},
		{
			name: "empty client area",
			snapshot: WindowSnapshot{
				Active: true,
			},
			wantErr:    true,
			wantReason: "клиентская область окна игры имеет некорректный размер",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateGDICaptureSnapshot(test.snapshot)
			if !test.wantErr {
				if err != nil {
					t.Fatalf("активное окно отклонено: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("небезопасное окно принято для GDI-захвата")
			}
			if !strings.Contains(err.Error(), "winplatform.validateGDICaptureSnapshot") ||
				!strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("неожиданная диагностика: %v", err)
			}
		})
	}
}
