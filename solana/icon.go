package solana

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/imaging"
	"github.com/MixinNetwork/safe/common"
	"github.com/chai2010/webp"
	"github.com/fogleman/gg"
)

const (
	size             = 512
	maxIconBytes     = 5 << 20
	maxIconDimension = 4096
	maxIconPixels    = 4 << 20
	imageHost        = "images.mixin.one"
)

func readImageFromUrl(ctx context.Context, url string) (*image.NRGBA, error) {
	if strings.HasSuffix(url, "=s128") {
		url = strings.TrimSuffix(url, "=s128") + "=s512"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "https" || req.URL.Host != imageHost || req.URL.User != nil {
		return nil, fmt.Errorf("icon URL must originate from https://images.mixin.one")
	}
	// RoundTrip performs one request without following redirects to another origin.
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("icon HTTP status: %d", res.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(contentType, "image/") {
		return nil, fmt.Errorf("invalid icon content type: %q", res.Header.Get("Content-Type"))
	}
	if res.ContentLength > maxIconBytes {
		return nil, fmt.Errorf("icon exceeds %d bytes", maxIconBytes)
	}

	data, err := io.ReadAll(io.LimitReader(res.Body, maxIconBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxIconBytes {
		return nil, fmt.Errorf("icon exceeds %d bytes", maxIconBytes)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxIconDimension || config.Height > maxIconDimension || config.Width > maxIconPixels/config.Height {
		return nil, fmt.Errorf("invalid icon dimensions: %dx%d", config.Width, config.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	icon512 := imaging.Resize(img, size, size, imaging.Lanczos)
	return icon512, nil
}

func applyCircleMask(img image.Image) image.Image {
	dc := gg.NewContext(size, size)
	dc.DrawRoundedRectangle(0, 0, size, size, size/2)
	dc.Clip()
	dc.DrawImage(img, 0, 0)
	return dc.Image()
}

func getWebpBase64(img image.Image) (string, error) {
	var buf bytes.Buffer
	err := webp.Encode(&buf, img, &webp.Options{
		Lossless: true,
		Exact:    true,
	})
	if err != nil {
		return "", err
	}
	base64Str := base64.StdEncoding.EncodeToString(buf.Bytes())
	return "data:image/webp;base64," + base64Str, nil
}

func (node *Node) processAssetIcon(ctx context.Context, asset *bot.AssetNetwork) (string, error) {
	icon, err := readImageFromUrl(ctx, asset.IconURL)
	if err != nil {
		return "", err
	}
	if asset.AssetID != bot.XINAssetId {
		mark, _, err := image.Decode(bytes.NewReader(FOOTMARK))
		if err != nil {
			return "", err
		}
		icon = imaging.Overlay(icon, mark, image.Pt(0, 0), 1.0)
	}

	circle := applyCircleMask(icon)
	imgBase64, err := getWebpBase64(circle)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]any{
		"content": imgBase64,
	})
	if err != nil {
		return "", err
	}

	traceId := common.UniqueId(node.group.GenesisId(), asset.AssetID)
	traceId = common.UniqueId(traceId, node.SafeUser().SpendPrivateKey)
	traceId = common.UniqueId(traceId, "icon")
	hash, err := common.CreateObjectStorageUntilSufficient(ctx, node.wallet, node.mixin, nil, data, traceId, *node.SafeUser())
	if err != nil {
		return "", err
	}
	iconUrl := fmt.Sprintf("https://kernel.mixin.dev/objects/%s/content", hash.String())
	err = node.store.UpdateExternalAssetIconUrl(ctx, asset.AssetID, iconUrl)
	if err != nil {
		return "", err
	}
	return iconUrl, nil
}

func (node *Node) checkExternalAssetUri(ctx context.Context, asset *bot.AssetNetwork) (string, error) {
	ea, err := node.store.ReadExternalAsset(ctx, asset.AssetID)
	if err != nil || ea == nil {
		return "", fmt.Errorf("invalid external asset to mint: %s", asset.AssetID)
	}
	if ea.Uri.Valid {
		return ea.Uri.String, nil
	}
	iconUrl, err := node.processAssetIcon(ctx, asset)
	if err != nil {
		return "", err
	}
	meta := solanaApp.Metadata{
		Name:        asset.Name,
		Symbol:      asset.Symbol,
		Description: fmt.Sprintf("%s bridged through Mixin Computer", asset.Name),
		Image:       iconUrl,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	traceId := common.UniqueId(node.group.GenesisId(), asset.AssetID)
	traceId = common.UniqueId(traceId, node.SafeUser().SpendPrivateKey)
	traceId = common.UniqueId(traceId, "metadata")
	hash, err := common.CreateObjectStorageUntilSufficient(ctx, node.wallet, node.mixin, nil, data, traceId, *node.SafeUser())
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://kernel.mixin.dev/objects/%s", hash.String())
	err = node.store.UpdateExternalAssetUri(ctx, ea.AssetId, url)
	if err != nil {
		return "", err
	}
	return url, nil
}
