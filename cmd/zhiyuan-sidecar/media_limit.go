package main

import (
	"errors"
	"fmt"

	"github.com/chenhg5/cc-connect/core"
)

func mediaMaxBytesFromOptions(options map[string]any) int64 {
	value := int64(defaultMediaMaxMB)
	switch raw := options["media_max_mb"].(type) {
	case int64:
		value = raw
	case int:
		value = int64(raw)
	case float64:
		value = int64(raw)
	}
	if value <= 0 {
		value = defaultMediaMaxMB
	}
	return value << 20
}

func validateInboundMedia(message *core.Message, maxBytes int64) error {
	if message == nil {
		return errors.New("message is required")
	}
	var total int64
	for _, image := range message.Images {
		total += int64(len(image.Data))
	}
	for _, file := range message.Files {
		total += int64(len(file.Data))
	}
	if message.Audio != nil {
		total += int64(len(message.Audio.Data))
	}
	if total > maxBytes {
		return fmt.Errorf("inbound media exceeds %d bytes", maxBytes)
	}
	return nil
}
