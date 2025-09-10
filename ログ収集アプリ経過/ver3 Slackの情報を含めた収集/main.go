package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

/********** 設定 **********/
const (
	pollInterval = 1500 * time.Millisecond
	// セッション（start/end）JSONの保存先
	logDir = "/Users/kmg2022-40/Desktop/activitylog/log"
	// Slack本文（1メッセージ=1JSON）の保存先
	messageDir = "/Users/kmg2022-40/Desktop/activitylog/Message"
)

/********** データ型 **********/
type record struct {
	App       string
	Title     string
	Activity  string
	Timestamp time.Time
}

type session struct {
	Start       string `json:"start"` // RFC3339
	End         string `json:"end"`   // RFC3339
	App         string `json:"app"`
	Title       string `json:"title"`
	Activity    string `json:"activity"`
	DurationSec int64  `json:"durationSec"` // 秒
}

type messageEntry struct {
	Timestamp string            `json:"timestamp"`
	Source    string            `json:"source"`              // "Slack"
	Direction string            `json:"direction,omitempty"` // "sent"
	Title     string            `json:"title,omitempty"`     // 例: チャンネルID/DM ID
	Text      string            `json:"text"`                // 本文
	Meta      map[string]string `json:"meta,omitempty"`      // channelId, threadTs, userId, ts など
}

/********** JSON配列ファイル ライター（セッション用） **********/
type jsonArrayWriter struct {
	path       string
	f          *os.File
	w          *bufio.Writer
	wroteFirst bool
}

func newJSONArrayWriter() (*jsonArrayWriter, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("activity_%s.json", time.Now().Format("20060102_150405"))
	path := filepath.Join(logDir, name)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriter(f)
	if _, err := w.WriteString("[\n"); err != nil {
		f.Close()
		return nil, err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return nil, err
	}
	return &jsonArrayWriter{path: path, f: f, w: w}, nil
}

func (j *jsonArrayWriter) AppendSession(s *session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if j.wroteFirst {
		if _, err := j.w.WriteString(",\n"); err != nil {
			return err
		}
	} else {
		j.wroteFirst = true
	}
	if _, err := j.w.Write(b); err != nil {
		return err
	}
	if err := j.w.Flush(); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *jsonArrayWriter) Close() error {
	if _, err := j.w.WriteString("\n]\n"); err != nil {
		return err
	}
	if err := j.w.Flush(); err != nil {
		return err
	}
	return j.f.Close()
}

/********** メイン **********/
func main() {
	fmt.Println("Activity logger (sessions + Slack self messages) started. Ctrl+C to stop.")

	jw, err := newJSONArrayWriter()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to prepare log: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Logging sessions to: %s\n", jw.path)

	// Slack取り込み（Socket Mode、自分の投稿のみ or 全保存デバッグ）をバックグラウンド起動
	go startSlackIngest()

	// 終了シグナルで最後のセッションを閉じる
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	var last *record
	var sessStart time.Time

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			app, title, err := frontmostAppAndTitleWithBrowserTabs()
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: %v\n", err)
				continue
			}
			activity := classifyActivity(app, title)
			now := time.Now()
			cur := &record{App: app, Title: title, Activity: activity, Timestamp: now}

			if last == nil {
				last = cur
				sessStart = now
				fmt.Printf("%s | start | %s | %s — %s\n",
					now.Format(time.RFC3339), last.Activity, last.App, short(last.Title, 80))
				continue
			}

			if changed(last, cur) {
				// 前セッションを確定
				s := sessionFrom(last, sessStart, now)
				if err := jw.AppendSession(&s); err != nil {
					fmt.Fprintf(os.Stderr, "log error: %v\n", err)
				} else {
					fmt.Printf("%s | end   | %s | dur=%ds\n",
						now.Format(time.RFC3339), last.Activity, s.DurationSec)
				}
				// 新しいセッション開始
				last = cur
				sessStart = now
				fmt.Printf("%s | start | %s | %s — %s\n",
					now.Format(time.RFC3339), last.Activity, last.App, short(last.Title, 80))
			}

		case <-sigCh:
			now := time.Now()
			if last != nil {
				s := sessionFrom(last, sessStart, now)
				if err := jw.AppendSession(&s); err != nil {
					fmt.Fprintf(os.Stderr, "log error(on exit): %v\n", err)
				} else {
					fmt.Printf("%s | end   | %s | dur=%ds (on exit)\n",
						now.Format(time.RFC3339), last.Activity, s.DurationSec)
				}
			}
			break loop
		}
	}

	if err := jw.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close error: %v\n", err)
	}
	fmt.Println("Stopped.")
}

/********** セッション化ユーティリティ **********/
func sessionFrom(r *record, start, end time.Time) session {
	dur := end.Sub(start).Round(time.Second)
	if dur < 0 {
		dur = 0
	}
	return session{
		Start:       start.Format(time.RFC3339),
		End:         end.Format(time.RFC3339),
		App:         clean(r.App),
		Title:       clean(r.Title),
		Activity:    clean(r.Activity),
		DurationSec: int64(dur / time.Second),
	}
}

/********** ブラウザのアクティブタブタイトル対応 **********/
func frontmostAppAndTitleWithBrowserTabs() (string, string, error) {
	// まず前面アプリ名
	appScript := `
		tell application "System Events"
			set frontApp to name of first process whose frontmost is true
		end tell
		return frontApp
	`
	app, err := runOSA(appScript)
	if err != nil {
		return "", "", fmt.Errorf("get frontmost app failed: %w", err)
	}
	app = strings.TrimSpace(app)
	low := strings.ToLower(app)

	// Safari：現在タブのタイトル
	if low == "safari" {
		title, e := runOSA(`
			tell application "Safari"
				try
					if (count of windows) > 0 then
						return name of current tab of front window
					else
						return ""
					end if
				on error
					return ""
				end try
			end tell
		`)
		if e == nil {
			return app, strings.TrimSpace(title), nil
		}
	}

	// Chromium系（Chrome/Edge/Brave/Vivaldi/Opera/Arc*）
	if isChromiumBrowser(low) {
		script := fmt.Sprintf(`
			tell application "%s"
				try
					if (count of windows) > 0 then
						return title of active tab of front window
					else
						return ""
					end if
				on error
					return ""
				end try
			end tell
		`, escapeOSA(app))
		title, e := runOSA(script)
		if e == nil {
			return app, strings.TrimSpace(title), nil
		}
	}

	// 取れない場合は従来のウインドウタイトル
	titleScript := fmt.Sprintf(`
		tell application "System Events"
			tell process "%s"
				try
					return name of front window
				on error
					try
						return value of attribute "AXTitle" of front window
					on error
						return ""
					end try
				end try
			end tell
		end tell
	`, escapeOSA(app))
	title, _ := runOSA(titleScript)
	return app, strings.TrimSpace(title), nil
}

func isChromiumBrowser(appLower string) bool {
	return strings.Contains(appLower, "chrome") ||
		strings.Contains(appLower, "edge") ||
		strings.Contains(appLower, "brave") ||
		strings.Contains(appLower, "vivaldi") ||
		strings.Contains(appLower, "opera") ||
		strings.Contains(appLower, "arc")
}

/********** AppleScript 実行 **********/
func runOSA(script string) (string, error) {
	cmd := exec.Command("osascript", "-e", script)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", errors.New(strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

/********** Slack 取り込み（Socket Mode、自分の投稿のみ or 全保存デバッグ） **********/
// 必要環境変数:
//   SLACK_BOT_TOKEN="xoxb-..."           （Bot Token）
//   SLACK_APP_TOKEN="xapp-..."           （App-level Token, scope: connections:write）
//   SLACK_SELF_USER_ID="UXXXXXXX"        （自分のSlackユーザーID。これと一致するユーザーの投稿だけ保存）
//   SLACK_DEBUG="1"                      （任意: 接続/イベントのデバッグ出力ON）
//   SLACK_LOG_ALL="1"                    （任意: 一時的に自分以外も保存＝イベント到達の切り分け）
func startSlackIngest() {
	bot := os.Getenv("SLACK_BOT_TOKEN")
	app := os.Getenv("SLACK_APP_TOKEN")
	self := os.Getenv("SLACK_SELF_USER_ID")
	debug := strings.TrimSpace(os.Getenv("SLACK_DEBUG")) == "1"
	logAll := strings.TrimSpace(os.Getenv("SLACK_LOG_ALL")) == "1"

	if bot == "" || app == "" {
		fmt.Fprintln(os.Stderr, "Slack ingest disabled: SLACK_BOT_TOKEN / SLACK_APP_TOKEN not set")
		return
	}
	if strings.TrimSpace(self) == "" {
		fmt.Fprintln(os.Stderr, "Slack ingest disabled: SLACK_SELF_USER_ID not set (自分のSlackユーザーIDを設定してください)")
		return
	}

	api := slack.New(
		bot,
		slack.OptionAppLevelToken(app),
		slack.OptionDebug(debug),
	)
	// Auth確認（ワークスペース/ボットIDの取り違いを早期検知）
	if at, err := api.AuthTest(); err == nil {
		fmt.Printf("[slack] auth ok: team=%s url=%s bot_user_id=%s\n", at.Team, at.URL, at.UserID)
	} else {
		fmt.Printf("[slack] auth test error: %v\n", err)
	}

	sm := socketmode.New(api)

	go func() {
		for evt := range sm.Events {
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				if debug {
					fmt.Println("[slack] connecting…")
				}
			case socketmode.EventTypeConnected:
				if debug {
					fmt.Println("[slack] connected")
				}
			case socketmode.EventTypeHello:
				if debug {
					fmt.Println("[slack] hello")
				}
			case socketmode.EventTypeEventsAPI:
				e, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					sm.Ack(*evt.Request)
					continue
				}
				sm.Ack(*evt.Request)

				if e.Type == slackevents.CallbackEvent {
					inner := e.InnerEvent
					switch ev := inner.Data.(type) {
					case *slackevents.MessageEvent:
						// サブタイプ（bot_message, message_changed など）は除外
						if ev.SubType != "" {
							if debug {
								fmt.Printf("[slack] drop subtype=%q user=%s ch=%s\n", ev.SubType, ev.User, ev.Channel)
							}
							break
						}
						// 本文が空は除外
						if strings.TrimSpace(ev.Text) == "" {
							if debug {
								fmt.Printf("[slack] drop empty text user=%s ch=%s\n", ev.User, ev.Channel)
							}
							break
						}
						if debug {
							fmt.Printf("[slack] msg user=%s ch=%s ts=%s text=%q\n", ev.User, ev.Channel, ev.TimeStamp, ev.Text)
						}
						// 自分だけ or 一時テストで全保存
						if !logAll && ev.User != self {
							if debug {
								fmt.Printf("[slack] drop not self (want=%s)\n", self)
							}
							break
						}
						m := messageEntry{
							Timestamp: time.Now().Format(time.RFC3339),
							Source:    "Slack",
							Direction: "sent",
							Title:     ev.Channel, // 例: Cxxxx / Dxxxx（チャンネル名解決は後で拡張可）
							Text:      ev.Text,
							Meta: map[string]string{
								"channelId": ev.Channel,
								"threadTs":  ev.ThreadTimeStamp,
								"userId":    ev.User,
								"ts":        ev.TimeStamp,
							},
						}
						if err := saveMessageJSON(m); err != nil {
							fmt.Fprintf(os.Stderr, "save slack msg error: %v\n", err)
						}
					}
				}
			case socketmode.EventTypeErrorBadMessage, socketmode.EventTypeErrorWriteFailed, socketmode.EventTypeDisconnect:
				fmt.Fprintf(os.Stderr, "socketmode error: %#v\n", evt)
			}
		}
	}()

	if err := sm.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "socketmode run error: %v\n", err)
	}
}

/********** Slackメッセージ保存 **********/
func saveMessageJSON(m messageEntry) error {
	if err := os.MkdirAll(messageDir, 0755); err != nil {
		return err
	}
	ts := time.Now().Format("20060102_150405.000")
	safe := func(s string) string {
		s = strings.ReplaceAll(s, " ", "_")
		s = strings.ReplaceAll(s, "/", "-")
		s = strings.ReplaceAll(s, "\\", "-")
		return s
	}
	fname := fmt.Sprintf("msg_%s_%s.json", ts, safe(m.Title))
	path := filepath.Join(messageDir, fname)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

/********** ラベリング・ヘルパ **********/
func classifyActivity(app, title string) string {
	a := strings.ToLower(app)
	t := strings.ToLower(title)

	// メール
	if a == "mail" || strings.Contains(a, "outlook") ||
		strings.Contains(t, "gmail") || strings.Contains(t, "outlook") || strings.Contains(t, "yahoo mail") {
		return "メールのやり取り"
	}

	// コーディング
	if strings.Contains(a, "visual studio code") || a == "xcode" ||
		strings.Contains(a, "intellij") || strings.Contains(a, "goland") {
		return "プログラムの制作"
	}
	for _, ext := range []string{".go", ".py", ".js", ".ts", ".rs", ".cpp", ".c", ".java", ".rb", ".kt", ".swift", ".cs"} {
		if strings.Contains(t, ext) {
			return "プログラムの制作"
		}
	}

	// コミュニケーション
	if strings.Contains(a, "slack") || strings.Contains(a, "teams") ||
		strings.Contains(a, "discord") || strings.Contains(a, "zoom") || strings.Contains(a, "meet") {
		return "コミュニケーション"
	}

	// ブラウザ
	if strings.Contains(a, "safari") || strings.Contains(a, "chrome") ||
		strings.Contains(a, "arc") || strings.Contains(a, "firefox") ||
		strings.Contains(a, "edge") || strings.Contains(a, "brave") ||
		strings.Contains(a, "opera") || strings.Contains(a, "vivaldi") {
		if hasAny(t, []string{"arxiv", "qiita", "stackoverflow", "docs", "doc:", "documentation", "mdn"}) {
			return "調査・ドキュメント閲覧"
		}
		return "Webブラウジング"
	}

	// ドキュメント/表計算/プレゼン
	if strings.Contains(a, "word") || strings.Contains(a, "pages") || strings.Contains(a, "notion") || strings.Contains(a, "obsidian") {
		return "ドキュメント編集"
	}
	if strings.Contains(a, "excel") || strings.Contains(a, "numbers") || strings.Contains(a, "sheets") {
		return "表計算・データ整理"
	}
	if strings.Contains(a, "powerpoint") || strings.Contains(a, "keynote") {
		return "プレゼン資料作成"
	}

	// ファイル操作
	if strings.Contains(a, "finder") || strings.Contains(a, "path finder") {
		return "ファイル操作"
	}

	// メディア
	if hasAny(t, []string{"youtube", "netflix", "twitch", "spotify", "music", "soundcloud"}) {
		return "メディア視聴・再生"
	}

	return "その他"
}

func hasAny(s string, keys []string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func changed(prev, cur *record) bool {
	if prev == nil {
		return true
	}
	// アプリ / タイトル / ラベル のどれかが変われば新しいセッションとみなす
	return prev.App != cur.App || prev.Title != cur.Title || prev.Activity != cur.Activity
}

var spaceRe = regexp.MustCompile(`\s+`)

func clean(s string) string {
	s = strings.ReplaceAll(s, "\u0000", "")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func short(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

func escapeOSA(s string) string {
	return strings.ReplaceAll(s, "\"", "\\\"")
}
