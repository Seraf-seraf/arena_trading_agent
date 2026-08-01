package winplatform

import "fmt"

func validateGDICaptureSnapshot(snapshot WindowSnapshot) error {
	const methodCtx = "winplatform.validateGDICaptureSnapshot"

	if snapshot.Minimized {
		return fmt.Errorf("%s: нельзя захватить свёрнутое окно игры", methodCtx)
	}
	if !snapshot.Active {
		return fmt.Errorf(
			"%s: GDI-захват фонового или перекрытого окна игры запрещён во избежание записи другого приложения",
			methodCtx,
		)
	}
	if snapshot.ClientArea.Width <= 0 || snapshot.ClientArea.Height <= 0 {
		return fmt.Errorf(
			"%s: клиентская область окна игры имеет некорректный размер %dx%d",
			methodCtx,
			snapshot.ClientArea.Width,
			snapshot.ClientArea.Height,
		)
	}
	return nil
}
