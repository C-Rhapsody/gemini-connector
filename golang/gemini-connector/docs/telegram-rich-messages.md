# Telegram LaTeX Rich Message Operator Guide

This document describes the configuration, operation, verification, and rollback procedures for the opt-in Telegram LaTeX Rich Message feature in `gemini-connector`.

---

## 1. Overview and Default-Off Policy

Telegram Bot API supports rich message formatting including native LaTeX math rendering via `<tg-math>` (inline) and `<tg-math-block>` (block) tags.

- **Default-off**: The feature is strictly disabled by default (`TELEGRAM_RICH_MESSAGES=false`). When disabled, the connector continues to use its standard Goldmark-to-HTML converter and ordinary message delivery pipeline.
- **Single Carrier**: Rich messages use HTML as their single carrier (`InputRichMessage.HTML`).
- **No SDK Changes**: The transport utilizes the existing `github.com/go-telegram-bot-api/telegram-bot-api/v5` via `BotAPI.MakeRequest` to call the `sendRichMessage` endpoint without third-party SDK migration.

---

## 2. Configuration Parameters

The feature is controlled by the following environment variables:

| Variable | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `TELEGRAM_RICH_MESSAGES` | boolean (`true`/`false`) | `false` | Enables the Rich Message delivery path. When `false`, all messages use ordinary HTML/plain delivery. |
| `TELEGRAM_RICH_MATH_ESCAPE` | string (`raw` or `numeric`) | None | Specifies the HTML entity escaping profile for math formulas. **Required** when `TELEGRAM_RICH_MESSAGES=true`. |
| `TELEGRAM_CHAT_ID` | integer (`int64`) | None | Configured Telegram chat ID. Must be non-zero when Rich is enabled. Rich delivery is strictly restricted to this target chat. |

### Escape Profiles

- **`numeric`** (Recommended): Encodes HTML-significant characters (`&` → `&#38;`, `<` → `&#60;`, `>` → `&#62;`) inside formulas to numeric entities. Compatible with formulas containing inequalities and matrices.
- **`raw`**: Leaves formula text unescaped. Formulas containing `<`, `>`, or `&` will fail closed to protect against HTML injection/corruption.

### Validation Rules

At startup, the configuration parser enforces fail-closed validation:
1. If `TELEGRAM_RICH_MESSAGES=true` and `TELEGRAM_RICH_MATH_ESCAPE` is missing or not one of `raw` or `numeric`, connector startup fails with an error.
2. If `TELEGRAM_RICH_MESSAGES=true` and `TELEGRAM_CHAT_ID` is 0 or missing, connector startup fails with an error.

---

## 3. Supported LaTeX Delimiters & Rules

The tokenizer inspects completed AI responses for the following delimiters:

- **Inline Math**: `\( ... \)` → renders as `<tg-math>...</tg-math>`
- **Block Math**: `\[ ... \]` and `$$ ... $$` → renders as `<tg-math-block>...</tg-math-block>`

### Code Protection

Formulas are **never** rendered inside code blocks. The tokenizer preserves the following verbatim:
- Fenced code blocks (` ``` ` or `~~~`)
- Indented code blocks (4 leading spaces or tab)
- Inline code spans (`` `...` `` or ```` ``...`` ````)
- Raw HTML blocks (`<pre>...</pre>` and `<code>...</code>`)

### Bare Dollar Protection

Bare dollar signs (such as currency `$50` or `$10.00`) are **never** treated as math delimiters. Only explicit `\(...\)`, `\[...]`, and `$$...$$` syntax triggers math rendering.

### Length Gate

Rendered Rich HTML is prefiltered by length in UTF-16 code units. If the post-restoration HTML exceeds 4,000 UTF-16 units, the message safely falls back to standard chunked delivery. Rich math candidates are never split mid-formula.

---

## 4. Scope of Application

Rich delivery is strictly restricted to terminal AI responses:
- **Eligible**: Completed AI assistant replies (where `SendOptions.AttachAfter` is non-zero and `SendOptions.Plain` is `false`).
- **Ineligible (Always Ordinary Delivery)**:
  - Command replies (`/help`, `/start`, `/status`, `/cron`, `/list`, `/switch`, etc.)
  - Cron scheduled task responses
  - Streaming chunk updates
  - Message edits (`editMessageText`)
  - Callback queries and keyboard interactions
  - Messages sent to chats other than the configured `TELEGRAM_CHAT_ID`

---

## 5. Fallback Contract & Idempotency

To prevent duplicate messages to users, the adapter implements a strict error classification contract:

| Error Category | Examples | Action | Duplicate Risk |
| :--- | :--- | :--- | :--- |
| **Definitive 4xx** | 400 Bad Request (e.g. malformed tag) | Fall back **once** to ordinary HTML/plain text delivery. | None (Telegram rejected the request). |
| **Method Not Found** | 404 Not Found (`method not found`) | Activates process-wide latch; permanently falls back to ordinary delivery for the remainder of the process. | None. |
| **Delivery-Uncertain** | 429 Too Many Requests, 5xx Server Error, TCP reset, client timeout, EOF, response decode failure | **Do NOT retry or send alternate format**. Return error immediately. | High if retried; prevented by contract. |

---

## 6. Operator Live Contract Probe

Before enabling `TELEGRAM_RICH_MESSAGES=true` in production, operators should execute the live contract probe to verify that the target Telegram bot and chat support the Rich API:

```bash
cd /path/to/gemini-connector/src
TELEGRAM_RICH_LIVE=1 \
TELEGRAM_BOT_TOKEN="<YOUR_BOT_TOKEN>" \
TELEGRAM_CHAT_ID="<YOUR_TARGET_CHAT_ID>" \
go test -v -run TestTelegramRich_LiveContract ./...
```

The probe exercises:
- Basic and nested Rich HTML structures (bold, italic, links, blockquotes, code).
- Inline math (mid-sentence, adjacent to formatting, multiple formulas).
- Block math (top-level, nested in blockquote and list items).
- Special characters (`<`, `>`, `&`) under `numeric` escape profile.
- Code block isolation.

> **Security Note**: Never commit bot tokens, chat IDs, or test output transcripts to version control.

---

## 7. Client Compatibility Matrix

While the Telegram Bot API accepts Rich messages server-side, client-side rendering depends on client platform version:

| Platform | Rendering Support | Stale Client Behavior |
| :--- | :--- | :--- |
| **Android** (v10.2+) | Native inline and block math rendering. | Displays raw text or degrades gracefully. |
| **iOS** (v10.2+) | Native inline and block math rendering. | Degrades gracefully. |
| **Desktop** (macOS / Windows / Linux) | Native inline and block math rendering. | Degrades gracefully. |
| **Telegram Web** (K / A) | Supports rich messages on modern versions. | Renders formula body text without math typography. |
| **Older / Stale Clients** | Untested / partial. | May display formula text without formatting. |

---

## 8. Rollback Procedure

If rendering issues, client incompatibilities, or Telegram API errors occur in production:

1. Set `TELEGRAM_RICH_MESSAGES=false` in the environment or `.env` file:
   ```env
   TELEGRAM_RICH_MESSAGES=false
   ```
2. Restart the `gemini-connector` process.
3. Verify that the connector resumes standard ordinary HTML/plain delivery immediately.

**No code rollback or binary rebuild is required to restore original behavior.**
