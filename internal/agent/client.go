// Package agent реализует локальный runtime Windows Agent.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

// Client поддерживает исходящее соединение Windows Agent с контроллером.
type Client struct {
	controllerURL string
	agentID       string
	version       string
	logger        *slog.Logger
}

// NewClient создаёт транспортный клиент агента.
func NewClient(controllerURL, agentID, version string, logger *slog.Logger) *Client {
	return &Client{controllerURL: controllerURL, agentID: agentID, version: version, logger: logger}
}

// Run подключается к контроллеру и отправляет heartbeat до отмены контекста.
func (c *Client) Run(ctx context.Context) error {
	logger := c.logger.With("метод", "Client.Run")
	conn, _, err := websocket.Dial(ctx, c.controllerURL, nil)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к контроллеру: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "агент завершает работу")

	if err := c.write(ctx, conn, protocol.MessageHello, protocol.Hello{AgentID: c.agentID, Version: c.version, Features: []string{"heartbeat"}}); err != nil {
		return err
	}
	logger.Info("соединение с контроллером установлено", "agent_id", c.agentID)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case at := <-ticker.C:
			if err := c.write(ctx, conn, protocol.MessageHeartbeat, protocol.Heartbeat{AgentID: c.agentID, At: at.UTC()}); err != nil {
				return err
			}
		}
	}
}

func (c *Client) write(ctx context.Context, conn *websocket.Conn, messageType protocol.MessageType, payload any) error {
	envelope, err := protocol.NewEnvelope(messageType, fmt.Sprintf("%s-%d", c.agentID, time.Now().UnixNano()), payload)
	if err != nil {
		return err
	}
	if err := wsjson.Write(ctx, conn, envelope); err != nil {
		return fmt.Errorf("не удалось отправить сообщение %s: %w", messageType, err)
	}
	return nil
}
