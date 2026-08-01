package winplatform_test

import (
	"github.com/arena-trading-agent/arena-trading-agent/internal/agent"
	winplatform "github.com/arena-trading-agent/arena-trading-agent/internal/platform/windows"
)

var (
	_ agent.WindowManager = (*winplatform.WindowManager)(nil)
	_ agent.CaptureDriver = (*winplatform.GDICapture)(nil)
	_ agent.InputDriver   = (*winplatform.SendInputDriver)(nil)
)
