package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Load reads, strictly decodes, normalizes and validates a runtime config.
// Relative detector paths are resolved against the runtime config directory.
func Load(path string) (*Runtime, error) {
	const methodCtx = "config.Load"

	if path == "" {
		return nil, fmt.Errorf("%s: путь к runtime-конфигурации не задан", methodCtx)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось определить путь к runtime-конфигурации: %w", methodCtx, err)
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось открыть runtime-конфигурацию: %w", methodCtx, err)
	}
	defer file.Close()

	value, err := Decode(file, filepath.Dir(absolutePath))
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось загрузить runtime-конфигурацию %q: %w", methodCtx, absolutePath, err)
	}
	return value, nil
}

// Decode strictly parses one JSON value. Unknown fields, trailing values and
// documents larger than MaxConfigBytes are rejected.
func Decode(reader io.Reader, baseDir string) (*Runtime, error) {
	const methodCtx = "config.Decode"

	if reader == nil {
		return nil, fmt.Errorf("%s: источник runtime-конфигурации не задан", methodCtx)
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось прочитать runtime-конфигурацию: %w", methodCtx, err)
	}
	if int64(len(data)) > MaxConfigBytes {
		return nil, fmt.Errorf("%s: runtime-конфигурация превышает %d байт", methodCtx, MaxConfigBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var value Runtime
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%s: не удалось декодировать runtime-конфигурацию: %w", methodCtx, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%s: после runtime-конфигурации найдено лишнее JSON-значение", methodCtx)
		}
		return nil, fmt.Errorf("%s: не удалось декодировать данные после runtime-конфигурации: %w", methodCtx, err)
	}

	if err := value.validate(baseDir); err != nil {
		return nil, fmt.Errorf("%s: runtime-конфигурация не прошла проверку: %w", methodCtx, err)
	}
	return &value, nil
}

// Validate normalizes and validates a programmatically created config. Relative
// detector paths remain relative; Load/Decode should be used for file configs.
func (r *Runtime) Validate() error {
	const methodCtx = "config.Runtime.Validate"

	if err := r.validate(""); err != nil {
		return fmt.Errorf("%s: runtime-конфигурация не прошла проверку: %w", methodCtx, err)
	}
	return nil
}
