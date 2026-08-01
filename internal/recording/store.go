// Package recording stores immutable visual evidence used by reconciliation
// and detector calibration.
package recording

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// FrameRecord describes an immutable frame artifact.
type FrameRecord struct {
	FrameID    uint64    `json:"frame_id"`
	Path       string    `json:"path"`
	SHA256     string    `json:"sha256"`
	Size       int       `json:"size"`
	Encoding   string    `json:"encoding"`
	CapturedAt time.Time `json:"captured_at"`
}

// ActionFrameEvidence links the immutable post-action frame to its command.
type ActionFrameEvidence struct {
	AgentID  string      `json:"agent_id"`
	ActionID string      `json:"action_id"`
	Frame    FrameRecord `json:"frame"`
}

// Store writes frame datasets under a private local directory.
type Store struct {
	root string
}

// New creates a recorder rooted at path.
func New(path string) (*Store, error) {
	const methodCtx = "recording.New"

	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%s: путь к хранилищу записей обязателен", methodCtx)
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return nil, fmt.Errorf("%s: не удалось создать каталог записей: %w", methodCtx, err)
	}
	return &Store{root: path}, nil
}

// SaveFrame atomically stores a frame and its metadata.
func (s *Store) SaveFrame(frame protocol.Frame) (FrameRecord, error) {
	const methodCtx = "recording.Store.SaveFrame"

	if len(frame.Data) == 0 {
		return FrameRecord{}, fmt.Errorf("%s: кадр %d пуст", methodCtx, frame.ID)
	}
	extension, err := extensionFor(frame.Encoding)
	if err != nil {
		return FrameRecord{}, fmt.Errorf("%s: не удалось определить расширение кадра: %w", methodCtx, err)
	}
	capturedAt := frame.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	}
	directory := filepath.Join(s.root, capturedAt.Format("2006-01-02"))
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return FrameRecord{}, fmt.Errorf("%s: не удалось создать каталог кадров: %w", methodCtx, err)
	}
	name := fmt.Sprintf("%s-%020d.%s", capturedAt.Format("150405.000000000"), frame.ID, extension)
	path := filepath.Join(directory, name)
	if err := atomicWrite(path, frame.Data, 0o640); err != nil {
		return FrameRecord{}, fmt.Errorf("%s: не удалось сохранить изображение кадра: %w", methodCtx, err)
	}
	sum := sha256.Sum256(frame.Data)
	record := FrameRecord{
		FrameID: frame.ID, Path: path, SHA256: hex.EncodeToString(sum[:]),
		Size: len(frame.Data), Encoding: frame.Encoding, CapturedAt: capturedAt,
	}
	metadata, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return FrameRecord{}, fmt.Errorf("%s: не удалось сериализовать метаданные кадра: %w", methodCtx, err)
	}
	if err := atomicWrite(path+".json", append(metadata, '\n'), 0o640); err != nil {
		return FrameRecord{}, fmt.Errorf("%s: не удалось сохранить метаданные кадра: %w", methodCtx, err)
	}
	return record, nil
}

// SaveObservation stores the normalized sidecar next to a previously recorded
// frame. The SQL repository remains the authoritative searchable state.
func (s *Store) SaveObservation(record FrameRecord, observation domain.Observation) error {
	const methodCtx = "recording.Store.SaveObservation"

	if observation.FrameID != record.FrameID {
		return fmt.Errorf(
			"%s: кадр наблюдения %d не совпадает с записью %d",
			methodCtx,
			observation.FrameID,
			record.FrameID,
		)
	}
	data, err := json.MarshalIndent(observation, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: не удалось сериализовать наблюдение: %w", methodCtx, err)
	}
	if err := atomicWrite(record.Path+".observation.json", append(data, '\n'), 0o640); err != nil {
		return fmt.Errorf("%s: не удалось сохранить наблюдение: %w", methodCtx, err)
	}
	return nil
}

// SaveActionFrame stores the exact frame embedded in ACTION_RESULT and writes
// a sidecar linking it to the durable action journal.
func (s *Store) SaveActionFrame(
	agentID string,
	actionID string,
	frame protocol.Frame,
) (FrameRecord, error) {
	const methodCtx = "recording.Store.SaveActionFrame"

	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(actionID) == "" {
		return FrameRecord{}, fmt.Errorf("%s: поля agent_id и action_id обязательны", methodCtx)
	}
	record, err := s.SaveFrame(frame)
	if err != nil {
		return FrameRecord{}, fmt.Errorf("%s: не удалось сохранить кадр действия: %w", methodCtx, err)
	}
	evidence, err := json.MarshalIndent(ActionFrameEvidence{
		AgentID:  agentID,
		ActionID: actionID,
		Frame:    record,
	}, "", "  ")
	if err != nil {
		return FrameRecord{}, fmt.Errorf("%s: не удалось сериализовать свидетельство действия: %w", methodCtx, err)
	}
	if err := atomicWrite(record.Path+".action.json", append(evidence, '\n'), 0o640); err != nil {
		return FrameRecord{}, fmt.Errorf("%s: не удалось сохранить свидетельство действия: %w", methodCtx, err)
	}
	return record, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	const methodCtx = "recording.atomicWrite"

	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".arena-recording-*")
	if err != nil {
		return fmt.Errorf("%s: не удалось создать временный файл записи: %w", methodCtx, err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return fmt.Errorf("%s: не удалось установить права временного файла: %w", methodCtx, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("%s: не удалось записать данные: %w", methodCtx, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("%s: не удалось синхронизировать запись: %w", methodCtx, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("%s: не удалось закрыть временный файл: %w", methodCtx, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("%s: не удалось опубликовать запись: %w", methodCtx, err)
	}
	return nil
}

func extensionFor(encoding string) (string, error) {
	const methodCtx = "recording.extensionFor"

	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "png", "image/png":
		return "png", nil
	case "jpg", "jpeg", "image/jpeg":
		return "jpg", nil
	case "webp", "image/webp":
		return "webp", nil
	default:
		return "", fmt.Errorf("%s: неподдерживаемая кодировка кадра %q", methodCtx, encoding)
	}
}
