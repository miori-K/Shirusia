package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Session struct {
	App      string
	BundleID string
	Title    string
	URL      string
	Category string
	Start    time.Time
}

type CSVLogger struct {
	file   *os.File
	writer *csv.Writer
	path   string
}

func main() {
	interval := 5 * time.Second

	// CSVロガー準備（起動のたびに新規ファイル）
	csvlog, err := newCSVLogger()
	if err != nil {
		log.Fatalf("csv init: %v", err)
	}
	defer csvlog.Close()
	log.Println("CSV:", csvlog.path)

	// Ctrl+C などで終了する時に最後のセッションを締める
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	var current *Session
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Println("ActivityLogger (CSV) started. Interval:", interval)

loop:
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			appName, bundleID, err := frontMostApp(ctx)
			cancel()
			if err != nil || appName == "" {
				log.Printf("frontMostApp error: %v", err)
				continue
			}

			title := ""
			url := ""

			switch appName {
			case "Safari":
				if t, u, err := safariFrontTab(); err == nil {
					title, url = t, u
				}
			case "Google Chrome":
				if t, u, err := chromeFrontTab("Google Chrome"); err == nil {
					title, url = t, u
				}
			case "Microsoft Edge":
				if t, u, err := chromeFrontTab("Microsoft Edge"); err == nil {
					title, url = t, u
				}
			}

			category := classify(appName, bundleID, title, url)
			now := time.Now()

			// セッションの切り替え判定（アプリ or カテゴリが変わったら締めて開始）
			if current != nil && (current.App != appName || current.Category != category) {
				// 前のセッションをend行で記録
				if err := csvlog.LogEnd(current, now); err != nil {
					log.Printf("csv end log err: %v", err)
				}
				current = nil
			}

			if current == nil {
				// 新規セッション開始
				current = &Session{
					App:      appName,
					BundleID: bundleID,
					Title:    truncate(title, 500),
					URL:      truncate(url, 1000),
					Category: category,
					Start:    now,
				}
				if err := csvlog.LogStart(current); err != nil {
					log.Printf("csv start log err: %v", err)
				} else {
					log.Printf("New session: %s [%s] %s (%s)", current.App, current.Category, current.Title, current.URL)
				}
			}
		case <-stop:
			// 終了時に進行中セッションがあればendを吐く
			if current != nil {
				if err := csvlog.LogEnd(current, time.Now()); err != nil {
					log.Printf("csv end log err (shutdown): %v", err)
				}
			}
			break loop
		}
	}

	log.Println("Bye.")
}

// ===== CSV Logger =====

func newCSVLogger() (*CSVLogger, error) {
	// 保存先ディレクトリを指定
	base := "/Users/kmg2022-40/Desktop/activitylog/log"
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}

	// 実行ごとに時刻入りの新規ファイルを作成
	ts := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("activity_%s.csv", ts)
	path := filepath.Join(base, filename)

	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := csv.NewWriter(f)

	// ヘッダー行
	if err := w.Write([]string{"timestamp", "event", "app", "bundle_id", "title", "url", "category"}); err != nil {
		f.Close()
		return nil, err
	}
	w.Flush()

	return &CSVLogger{file: f, writer: w, path: path}, nil
}

func (c *CSVLogger) LogStart(s *Session) error {
	row := []string{
		s.Start.Format(time.RFC3339),
		"start",
		s.App,
		s.BundleID,
		s.Title,
		s.URL,
		s.Category,
	}
	if err := c.writer.Write(row); err != nil {
		return err
	}
	c.writer.Flush()
	return c.writer.Error()
}

func (c *CSVLogger) LogEnd(s *Session, end time.Time) error {
	row := []string{
		end.Format(time.RFC3339),
		"end",
		s.App,
		s.BundleID,
		s.Title,
		s.URL,
		s.Category,
	}
	if err := c.writer.Write(row); err != nil {
		return err
	}
	c.writer.Flush()
	return c.writer.Error()
}

func (c *CSVLogger) Close() error {
	if c.writer != nil {
		c.writer.Flush()
	}
	if c.file != nil {
		return c.file.Close()
	}
	return nil
}

// ===== 分類・補助 =====

func classify(appName, bundleID, title, url string) string {
	n := strings.ToLower(appName + " " + bundleID + " " + title + " " + url)

	// メール
	if strings.Contains(n, "com.apple.mail") || appName == "Mail" {
		return "メールのやり取り"
	}
	if strings.Contains(n, "mail.google") || strings.Contains(n, "gmail") ||
		strings.Contains(n, "outlook") || strings.Contains(n, "icloud.com/mail") ||
		strings.Contains(n, "mail.yahoo") {
		return "メールのやり取り"
	}

	// プログラミング
	if strings.Contains(n, "com.microsoft.vscode") || appName == "Visual Studio Code" || appName == "Code" {
		return "プログラムの制作"
	}
	if strings.Contains(n, "com.apple.dt.xcode") || appName == "Xcode" {
		return "プログラムの制作"
	}
	if strings.Contains(n, "github.com") || strings.Contains(n, "gitlab.com") {
		return "開発プラットフォーム"
	}

	// ドキュメント/資料
	if strings.Contains(n, "docs.google.com") || strings.Contains(n, "notion.so") || strings.Contains(n, "dropbox.com") {
		return "ドキュメント作業"
	}

	// 学習/検索/動画
	if strings.Contains(n, "stackoverflow.com") || strings.Contains(n, "stack overflow") {
		return "技術調査"
	}
	if strings.Contains(n, "youtube.com") || strings.Contains(n, "youtu.be") || strings.Contains(n, "nicovideo.jp") {
		return "動画視聴"
	}

	// ブラウジング
	if appName == "Safari" || appName == "Google Chrome" || appName == "Microsoft Edge" {
		return "ブラウジング"
	}

	return "その他"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// ===== AppleScript =====

func frontMostApp(ctx context.Context) (string, string, error) {
	appName, err := runOSA(ctx, `tell application "System Events" to get name of (first process whose frontmost is true)`)
	if err != nil {
		return "", "", err
	}
	appName = strings.TrimSpace(appName)

	bundleID, err := runOSA(ctx, fmt.Sprintf(`id of application "%s"`, escapeAS(appName)))
	if err != nil {
		bundleID = ""
	}
	return appName, strings.TrimSpace(bundleID), nil
}

func safariFrontTab() (string, string, error) {
	s := `
tell application "Safari"
	try
		if (count of windows) > 0 and exists (front document) then
			set t to name of front document
			set u to URL of front document
			return t & "
" & u
		else
			return ""
		end if
	on error
		return ""
	end try
end tell`
	out, err := runOSA(context.Background(), s)
	if err != nil || strings.TrimSpace(out) == "" {
		return "", "", fmt.Errorf("no safari tab")
	}
	parts := strings.SplitN(out, "\n", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
	}
	return strings.TrimSpace(out), "", nil
}

func chromeFrontTab(app string) (string, string, error) {
	s := fmt.Sprintf(`
tell application "%s"
	try
		if (count of windows) > 0 then
			set t to title of active tab of front window
			set u to URL of active tab of front window
			return t & "
" & u
		else
			return ""
		end if
	on error
		return ""
	end try
end tell`, escapeAS(app))
	out, err := runOSA(context.Background(), s)
	if err != nil || strings.TrimSpace(out) == "" {
		return "", "", fmt.Errorf("no chrome-like tab")
	}
	parts := strings.SplitN(out, "\n", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
	}
	return strings.TrimSpace(out), "", nil
}

func runOSA(ctx context.Context, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("osascript: %v (%s)", err, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func escapeAS(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}
