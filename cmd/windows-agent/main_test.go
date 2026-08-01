package main

import "testing"

func TestValidateSafetyFlagsRequiresCalibrationForInput(t *testing.T) {
	t.Parallel()
	if err := validateSafetyFlags(true, ""); err == nil {
		t.Fatal("ввод без откалиброванного screen-config должен быть запрещён")
	}
	if err := validateSafetyFlags(true, "screens.json"); err != nil {
		t.Fatalf("безопасная конфигурация отклонена: %v", err)
	}
	if err := validateSafetyFlags(false, ""); err != nil {
		t.Fatalf("диагностический режим без SendInput отклонён: %v", err)
	}
}
