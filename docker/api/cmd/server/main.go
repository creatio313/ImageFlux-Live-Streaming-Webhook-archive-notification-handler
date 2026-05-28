package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	archiveCreatedType = "imageflux.archive_created"
	archiveFailedType  = "imageflux.archive_failed"
	targetFileType     = "m3u8"
)

const servicePrincipalTokenEndpoint = "https://secure.sakura.ad.jp/cloud/api/iam/1.0/service-principals/oauth2/token"

const simpleNotificationEndpointFmt = "https://secure.sakura.ad.jp/cloud/zone/is1a/api/cloud/1.1/commonserviceitem/%s/simplenotification/message"

// アーカイブ作成時のイベントWebhook通知の構造
type WebhookPayload struct {
	ChannelID string      `json:"channel_id"`
	Type      string      `json:"type"`
	Data      WebhookData `json:"data"`
}

// アーカイブ作成時のイベントWebhook通知のうち、dataの子項目
type WebhookData struct {
	DestURI     string `json:"dest_uri"`
	FilePath    string `json:"file_path"`
	Size        int64  `json:"size"`
	FileType    string `json:"file_type"`
	AbsoluteURL string `json:"absolute_url"`
}

type simpleNotificationPayload struct {
	Message string `json:"Message"`
}

type simpleNotificationClient struct {
	kid                string
	servicePrincipalID string
	privateKeyPEM      string
	notificationID     string
	httpClient         *http.Client
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	notifier, err := newSimpleNotificationClientFromEnv()
	if err != nil {
		log.Fatalf("シンプル通知APIの初期化に失敗しました: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", webhookHandler(notifier))

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	log.Printf("サーバが%sポートで起動しました。", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("サーバの起動に失敗しました: %v", err)
	}
}

func newSimpleNotificationClientFromEnv() (*simpleNotificationClient, error) {
	kid := os.Getenv("SERVICE_PRINCIPAL_KEY_KID")
	servicePrincipalID := os.Getenv("SERVICE_PRINCIPAL_RESOURCE_ID")
	privateKeyPEM, err := loadPrivateKeyPEMFromEnv()
	if err != nil {
		return nil, err
	}
	notificationID := os.Getenv("NOTIFICATION_GROUP_ID")

	if kid == "" {
		return nil, fmt.Errorf("環境変数SERVICE_PRINCIPAL_KEY_KIDが設定されていません")
	}
	if servicePrincipalID == "" {
		return nil, fmt.Errorf("環境変数SERVICE_PRINCIPAL_RESOURCE_IDが設定されていません")
	}
	if notificationID == "" {
		return nil, fmt.Errorf("環境変数NOTIFICATION_GROUP_IDが設定されていません")
	}

	return &simpleNotificationClient{
		kid:                kid,
		servicePrincipalID: servicePrincipalID,
		privateKeyPEM:      privateKeyPEM,
		notificationID:     notificationID,
		httpClient:         &http.Client{},
	}, nil
}
//分割した秘密鍵環境変数を再ロード
func loadPrivateKeyPEMFromEnv() (string, error) {
	// 互換性のため、単一環境変数があれば優先して利用する。
	if pemValue := os.Getenv("SERVICE_PRINCIPAL_PRIVATE_KEY_PEM"); pemValue != "" {
		return pemValue, nil
	}

	chunkCountRaw := os.Getenv("SERVICE_PRINCIPAL_PRIVATE_KEY_PEM_B64_CHUNK_COUNT")
	if chunkCountRaw == "" {
		return "", fmt.Errorf("環境変数SERVICE_PRINCIPAL_PRIVATE_KEY_PEMまたはSERVICE_PRINCIPAL_PRIVATE_KEY_PEM_B64_CHUNK_COUNTが設定されていません")
	}

	chunkCount, err := strconv.Atoi(chunkCountRaw)
	if err != nil || chunkCount <= 0 {
		return "", fmt.Errorf("環境変数SERVICE_PRINCIPAL_PRIVATE_KEY_PEM_B64_CHUNK_COUNTが不正です")
	}

	var b64Builder strings.Builder
	for i := 0; i < chunkCount; i++ {
		chunkName := fmt.Sprintf("SERVICE_PRINCIPAL_PRIVATE_KEY_PEM_B64_%03d", i)
		chunkValue := os.Getenv(chunkName)
		if chunkValue == "" {
			return "", fmt.Errorf("環境変数%sが設定されていません", chunkName)
		}
		b64Builder.WriteString(chunkValue)
	}

	decoded, err := base64.StdEncoding.DecodeString(b64Builder.String())
	if err != nil {
		return "", fmt.Errorf("秘密鍵(base64分割)の復元に失敗しました: %w", err)
	}

	return string(decoded), nil
}
func webhookHandler(notifier *simpleNotificationClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("リクエストボディの読み取りに失敗しました: %v", err)
			http.Error(w, "リクエストボディの読み取りに失敗しました", http.StatusBadRequest)
			return
		}

		// フィルタリング結果のいかんを問わず、Webhook通知の内容を標準出力する。
		log.Printf("received webhook raw body: %s", string(body))

		// JSONの構造が不正な場合はエラーを返す。
		var payload WebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("無効なJSONです: %v", err)
			http.Error(w, "無効なJSONです", http.StatusBadRequest)
			return
		}

		if payload.Type != archiveCreatedType && payload.Type != archiveFailedType {
			log.Printf("type値%qはフィルタリング対象外のため、後続の処理を省略します。", payload.Type)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"matched": false,
			})
			return
		}

		// フィルタリング対象のtype値の場合は内容を変数化して判定する。
		channelID := payload.ChannelID
		eventType := payload.Type
		destURI := payload.Data.DestURI
		filePath := payload.Data.FilePath
		size := payload.Data.Size
		fileType := payload.Data.FileType
		absoluteURL := payload.Data.AbsoluteURL

		log.Printf(
			"Webhookを処理します: チャンネルID：%q タイプ：%q アーカイブ保存先URI：%q ファイルパス：%q ファイルサイズ：%d ファイル形式：%q 復元URL（欠損時のみ）：%q",
			channelID,
			eventType,
			destURI,
			filePath,
			size,
			fileType,
			absoluteURL,
		)

		fileTypeMatched := fileType == targetFileType
		matched := (eventType == archiveCreatedType && fileTypeMatched) || eventType == archiveFailedType
		if matched {
			log.Printf("通知要件に合致しました。")

			var message string
			if eventType == archiveCreatedType {
				message = fmt.Sprintf(
					"アーカイブが作成されました: チャンネルID：%q アーカイブ保存先URI：%q ファイルパス：%q ファイルサイズ：%d ファイル形式：%q",
					channelID,
					destURI,
					filePath,
					size,
					fileType,
				)
			} else {
				message = fmt.Sprintf(
					"アーカイブの作成に失敗しました: チャンネルID：%q ファイルパス：%q 復元URL（通知から10分間のみ有効/配信中に限る）：%q",
					channelID,
					filePath,
					absoluteURL,
				)
			}

			if err := notifier.send(r.Context(), message); err != nil {
				log.Printf("シンプル通知APIの呼び出しに失敗しました: %v", err)
				http.Error(w, "シンプル通知APIの呼び出しに失敗しました", http.StatusInternalServerError)
				return
			}
		} else {
			log.Printf("通知要件に合致しませんでした。")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"matched": matched,
		})
	}
}
func (c *simpleNotificationClient) send(ctx context.Context, message string) error {
	accessToken, err := c.issueAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("サービスプリンシパルアクセストークンの発行に失敗しました: %w", err)
	}

	payloadBody, err := json.Marshal(simpleNotificationPayload{Message: message})
	if err != nil {
		return fmt.Errorf("通知メッセージのJSON化に失敗しました: %w", err)
	}

	endpoint := fmt.Sprintf(simpleNotificationEndpointFmt, c.notificationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBody))
	if err != nil {
		return fmt.Errorf("通知リクエストの生成に失敗しました: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("通知API呼び出しに失敗しました: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("通知APIレスポンスの読み取りに失敗しました: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("通知APIがエラーを返却しました: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	log.Printf("シンプル通知API呼び出しに成功しました: status=%d", resp.StatusCode)
	return nil
}
/***
以降、サービスプリンシパルキーを用いてアクセストークンを発行する処理の実装。
***/
func (c *simpleNotificationClient) issueAccessToken(ctx context.Context) (string, error) {
	//JWTを生成
	assertion, err := c.createSignedJWT()
	if err != nil {
		return "", fmt.Errorf("JWTの生成に失敗しました: %w", err)
	}

	//アクセストークンの発行APIを呼び出す
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, servicePrincipalTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("トークン発行リクエストの生成に失敗しました: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("トークン発行API呼び出しに失敗しました: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("トークン発行レスポンスの読み取りに失敗しました: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("トークン発行APIがエラーを返却しました: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return "", fmt.Errorf("トークン発行レスポンスのJSON解析に失敗しました: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("トークン発行レスポンスにaccess_tokenがありません")
	}

	return tokenResp.AccessToken, nil
}
func (c *simpleNotificationClient) createSignedJWT() (string, error) {
	now := time.Now().UTC().Unix()

	header := map[string]string{
		"alg": "RS256",
		"kid": c.kid,
		"typ": "JWT",
	}
	payload := map[string]any{
		"aud": servicePrincipalTokenEndpoint,
		"exp": now + 300,
		"iat": now,
		"iss": c.servicePrincipalID,
		"sub": c.servicePrincipalID,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("JWTヘッダーのJSON化に失敗しました: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("JWTペイロードのJSON化に失敗しました: %w", err)
	}

	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	dataToSign := encodedHeader + "." + encodedPayload

	//署名用秘密鍵の読み取りとパース
	privateKey, err := parseRSAPrivateKey(c.privateKeyPEM)
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256([]byte(dataToSign))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("JWT署名に失敗しました: %w", err)
	}

	encodedSignature := base64.RawURLEncoding.EncodeToString(signature)
	return dataToSign + "." + encodedSignature, nil
}

func parseRSAPrivateKey(privateKeyPEM string) (*rsa.PrivateKey, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(privateKeyPEM), `\n`, "\n")
	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return nil, fmt.Errorf("秘密鍵PEMのデコードに失敗しました")
	}

	if pkcs1Key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return pkcs1Key, nil
	}

	pkcs8Key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("秘密鍵の解析に失敗しました: %w", err)
	}

	rsaKey, ok := pkcs8Key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("RSA秘密鍵ではありません")
	}

	return rsaKey, nil
}