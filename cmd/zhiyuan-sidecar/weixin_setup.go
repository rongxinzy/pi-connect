package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const weixinSetupBaseURL = "https://ilinkai.weixin.qq.com"

type weixinSetupRequest struct {
	QRCode string `json:"qrcode"`
}

type weixinSetupResponse struct {
	Status      string `json:"status"`
	QRCode      string `json:"qrcode,omitempty"`
	QRCodeURL   string `json:"qrcodeUrl,omitempty"`
	BotToken    string `json:"botToken,omitempty"`
	AccountID   string `json:"accountId,omitempty"`
	BaseURL     string `json:"baseUrl,omitempty"`
	IlinkUserID string `json:"userId,omitempty"`
}

func runWeixinSetupCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: cc-connect-sidecar weixin-setup <start|poll>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var result weixinSetupResponse
	var err error
	switch args[0] {
	case "start":
		result, err = startWeixinSetup(ctx)
	case "poll":
		var request weixinSetupRequest
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&request); decodeErr != nil || strings.TrimSpace(request.QRCode) == "" {
			return errors.New("invalid Weixin setup poll request")
		}
		result, err = pollWeixinSetup(ctx, request.QRCode)
	default:
		return errors.New("unknown Weixin setup action")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func startWeixinSetup(ctx context.Context) (weixinSetupResponse, error) {
	endpoint, _ := url.Parse(weixinSetupBaseURL + "/ilink/bot/get_bot_qrcode")
	query := endpoint.Query()
	query.Set("bot_type", "3")
	endpoint.RawQuery = query.Encode()
	var response struct {
		QRCode           string `json:"qrcode"`
		QRCodeImgContent string `json:"qrcode_img_content"`
	}
	if err := getWeixinSetupJSON(ctx, endpoint.String(), &response); err != nil {
		return weixinSetupResponse{}, fmt.Errorf("get Weixin QR code: %w", err)
	}
	if strings.TrimSpace(response.QRCode) == "" || strings.TrimSpace(response.QRCodeImgContent) == "" {
		return weixinSetupResponse{}, errors.New("Weixin QR response is incomplete")
	}
	return weixinSetupResponse{Status: "wait", QRCode: response.QRCode, QRCodeURL: response.QRCodeImgContent}, nil
}

func pollWeixinSetup(ctx context.Context, qrCode string) (weixinSetupResponse, error) {
	endpoint, _ := url.Parse(weixinSetupBaseURL + "/ilink/bot/get_qrcode_status")
	query := endpoint.Query()
	query.Set("qrcode", strings.TrimSpace(qrCode))
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return weixinSetupResponse{}, err
	}
	request.Header.Set("iLink-App-ClientVersion", "1")
	var response struct {
		Status      string `json:"status"`
		BotToken    string `json:"bot_token"`
		IlinkBotID  string `json:"ilink_bot_id"`
		BaseURL     string `json:"baseurl"`
		IlinkUserID string `json:"ilink_user_id"`
	}
	if err := doWeixinSetupJSON(request, &response); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return weixinSetupResponse{Status: "wait"}, nil
		}
		return weixinSetupResponse{}, fmt.Errorf("poll Weixin QR code: %w", err)
	}
	return weixinSetupResponse{
		Status: response.Status, BotToken: response.BotToken, AccountID: response.IlinkBotID,
		BaseURL: response.BaseURL, IlinkUserID: response.IlinkUserID,
	}, nil
}

func getWeixinSetupJSON(ctx context.Context, rawURL string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	return doWeixinSetupJSON(request, target)
}

func doWeixinSetupJSON(request *http.Request, target any) error {
	response, err := (&http.Client{Timeout: 38 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target)
}
