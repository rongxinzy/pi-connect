package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

const maxBridgeAttachmentBytes = 100 << 20

type bridgeAttachment struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	MimeType string `json:"mimeType,omitempty"`
	FileName string `json:"fileName,omitempty"`
}

type bridgeTurnResponse struct {
	Content     string             `json:"content"`
	Attachments []bridgeAttachment `json:"attachments,omitempty"`
}

func sendBridgeTurnResponse(ctx context.Context, platform core.Platform, replyCtx any, response bridgeTurnResponse) error {
	if strings.TrimSpace(response.Content) == "" && len(response.Attachments) == 0 {
		return errors.New("ZhiYuan bridge returned an empty response")
	}
	if strings.TrimSpace(response.Content) != "" {
		if err := platform.Reply(ctx, replyCtx, response.Content); err != nil {
			return fmt.Errorf("send text reply: %w", err)
		}
	}
	for _, attachment := range response.Attachments {
		if err := sendBridgeAttachment(ctx, platform, replyCtx, attachment); err != nil {
			return err
		}
	}
	return nil
}

func sendBridgeAttachment(ctx context.Context, platform core.Platform, replyCtx any, attachment bridgeAttachment) error {
	path := filepath.Clean(strings.TrimSpace(attachment.Path))
	if path == "." || !filepath.IsAbs(path) {
		return errors.New("bridge attachment path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("inspect bridge attachment: %w", err)
	}
	if !sameCleanPath(path, resolved) {
		return errors.New("bridge attachment must be a regular file, not a link")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open bridge attachment: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect open bridge attachment: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("bridge attachment must be a regular file")
	}
	if info.Size() > maxBridgeAttachmentBytes {
		return fmt.Errorf("bridge attachment exceeds %d bytes", maxBridgeAttachmentBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBridgeAttachmentBytes+1))
	if err != nil {
		return fmt.Errorf("read bridge attachment: %w", err)
	}
	if len(data) > maxBridgeAttachmentBytes {
		return fmt.Errorf("bridge attachment exceeds %d bytes", maxBridgeAttachmentBytes)
	}
	fileName := strings.TrimSpace(attachment.FileName)
	if fileName == "" {
		fileName = filepath.Base(path)
	}
	mimeType := strings.TrimSpace(attachment.MimeType)
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(fileName))
	}
	if attachment.Kind == "image" {
		sender, ok := platform.(core.ImageSender)
		if !ok {
			return errors.New("platform does not support image delivery")
		}
		return sender.SendImage(ctx, replyCtx, core.ImageAttachment{Data: data, MimeType: mimeType, FileName: fileName})
	}
	sender, ok := platform.(core.FileSender)
	if !ok {
		return errors.New("platform does not support file delivery")
	}
	return sender.SendFile(ctx, replyCtx, core.FileAttachment{Data: data, MimeType: mimeType, FileName: fileName})
}

func sameCleanPath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
