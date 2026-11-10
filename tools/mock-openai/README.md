# mock-openai

A small OpenAI-compatible HTTP server (Go standard library only) for testing Bifrost
locally without any paid API key. It answers chat completions deterministically and
returns deterministic embeddings, and it counts every call so a test can tell whether
a request reached the "provider" or was served from Bifrost's cache.

> **This is not a semantic model.** The embeddings are a bag-of-words hashing trick.
> Two texts are "similar" only when they share content words (or most of the letters
> of their words). Synonyms, word order, negation and meaning are invisible to it.
> It exists to exercise the mechanics of a semantic cache (embedding call, vector
> storage, nearest-neighbour search, threshold), not to show cache quality. Do not
> quote hit rates or similarity scores measured with it as evidence about real
> embedding models.

## Build, test, run

```powershell
$env:Path = "C:\Program Files\Go\bin;" + $env:Path      # Go is not on PATH in new shells here
cd C:\Madhusudhan\Geeks\hackathon-workspace\tools\mock-openai
go test -count=1 -v ./...
go build -o ..\..\e2e\bin\mock-openai.exe .
..\..\e2e\bin\mock-openai.exe -port 18080 -dim 256 -api-key sk-mock-local
```

| Flag | Env var | Default | Meaning |
|---|---|---|---|
| `-host` | `MOCK_OPENAI_HOST` | `127.0.0.1` | Bind address. Use `0.0.0.0` if a Docker container must reach it via `host.docker.internal`. |
| `-port` | `MOCK_OPENAI_PORT` | `18080` | Listen port. |
| `-dim` | `MOCK_OPENAI_DIM` | `256` | Embedding dimension (1 to 16000). A request's `dimensions` field overrides it per call. |
| `-api-key` | `MOCK_OPENAI_API_KEY` | empty | When set, `/v1/*` requires `Authorization: Bearer <key>` (401 otherwise). Use a mock value such as `sk-mock-local`. |
| `-chat-latency` | `MOCK_OPENAI_CHAT_LATENCY` | `0` | Artificial delay before each chat completion (e.g. `100ms`), to make cache hits visibly faster. |
| `-models` | `MOCK_OPENAI_MODELS` | `mock-chat,mock-embedding` | Ids listed by `GET /v1/models`. Requests accept any model id. |
| `-knowledge` | `MOCK_OPENAI_KNOWLEDGE` | empty | JSON file of canned chat answers for questions asked without context lines (see [Knowledge file](#knowledge-file)). Empty: off. A bad file stops the server at startup. |

## Endpoints

| Method and path | Behaviour |
|---|---|
| `POST /v1/chat/completions` | OpenAI `chat.completion` response with `usage`. `stream: true` is supported: `chat.completion.chunk` SSE events (role chunk, one chunk per word, finish chunk, a usage chunk when `stream_options.include_usage` is true, then `data: [DONE]`). `n > 1` is rejected with 400. |
| `POST /v1/embeddings` | OpenAI `list` of `embedding` objects with `usage`. `input` may be a string, an array of strings, an array of token ids, or an array of token-id arrays (token ids are embedded as the words `tok<id>`). `encoding_format` `float` (default) or `base64` (little-endian float32). Empty input is rejected with 400, as OpenAI does. |
| `GET /v1/models`, `GET /v1/models/{id}` | Minimal model objects. |
| `GET /mock/stats` | `{"chat_calls", "chat_stream_calls", "embedding_calls", "embedding_inputs", "models_calls", "rejected_calls", "unknown_calls", "dimension", "started_at"}`. Only successful calls are counted in `chat_calls` and `embedding_calls` (streaming calls are included in `chat_calls`). |
| `POST /mock/reset` | Zeroes the counters and clears the request log. Completion ids are never reused (they keep counting). |
| `GET /mock/requests[?kind=chat\|chat_stream\|embedding\|models\|unknown]` | The last 200 requests: kind, model, status, completion id, the user question, the knowledge rule that answered (`rule`, omitted when none did), and the exact embedding input texts. |
| `POST /mock/similarity` | `{"a": "...", "b": "..."}` returns the cosine of the two mock embeddings and their content words. |
| `GET /health` | `{"status":"ok"}`. |
| anything else | 404 in OpenAI error shape, counted in `unknown_calls` (useful to spot unexpected gateway calls). |

Errors use the OpenAI shape: `{"error": {"message", "type", "param", "code"}}`.

## Chat answers

The reply is built from the last `user` message (the question) and the `system`
(or `developer`) messages:

- No context lines in the system prompt: `Mock answer to: <question>`, or the answer of the first matching rule when a [knowledge file](#knowledge-file) is loaded.
- Context lines present: `According to the provided information, <first context line that shares a content word with the question>`.
- Context lines present but none shares a content word: `The provided information does not answer: <question>`.

A context line is a list item in the system prompt (`- `, `* `, `• `, `1. `, `1) `,
`[1] `, marker removed) or any non-empty line after a `Context:` heading. Content
words are the tokens left after the stopword filter below (exact token match, no
stemming). Completion ids are `chatcmpl-mock-000001`, `-000002`, ... so a test can
tell which upstream call produced a cached response.

### Knowledge file

A knowledge file gives the mock canned answers, so that it behaves like a real model in
a "wrong match" demo: questions that mean the same thing get the identical answer, and
look-alike questions that mean different things ("turn on" vs "turn off" two-factor
login) get clearly different answers. Without one, both kinds get the echo
`Mock answer to: <question>`, which differs whenever the wording does. It is off by
default.

```powershell
..\..\e2e\bin\mock-openai.exe -port 18080 -knowledge ..\..\docker\mock-knowledge.json
```

```json
{
  "_comment": "optional, here and on each rule",
  "rules": [
    {"name": "two-factor-on", "all": ["two", "factor"], "any": ["on", "enable"], "none": ["off", "disable"],
     "answer": "To turn on two-factor login, open Settings > Security > Two-factor login and tap Enable, ..."},
    {"name": "two-factor-off", "all": ["two", "factor"], "any": ["off", "disable"],
     "answer": "To turn off two-factor login, open Settings > Security > Two-factor login and tap Disable. ..."}
  ]
}
```

- A rule matches when the question (the last `user` message) contains every `all` word,
  at least one `any` word (if `any` is given) and no `none` word. Words are compared with
  every token of the question (step 1 of the embedding algorithm below), stopwords
  included, so `on` and `off` select different rules although the embedding ignores both.
- Rules are tried in file order and the first match wins; its `answer` is returned verbatim.
- Context lines take precedence: when the system prompt has any, the rules are not
  consulted. A question that no rule matches gets `Mock answer to: <question>`.
- `GET /mock/requests` shows which rule answered a chat call, as `rule`.
- The file is checked at startup, and a bad file stops the server with a message that
  names the file and the rule: an unknown field (`_comment` is allowed), a missing or
  duplicate `name`, a blank `answer`, a rule with no `all` or `any` word, a word that is
  not a single token (`two factor`, `what's`: it could never match), or a word in both
  `none` and `all`/`any`. Words are lowercased and trimmed when loaded.

`docker/mock-knowledge.json` is the knowledge file of the dashboard's wrong-match demo;
`TestDemoKnowledgeFile` pins its answers.

## Embedding algorithm

1. Lowercase the text and split it into maximal runs of letters and digits.
2. Drop stopwords: about 120 English function words (`the`, `is`, `what`, `of`, ...),
   single letters, and the role labels `user`, `system`, `assistant`, `developer`
   (Bifrost's semantic cache embeds each message as `role: content`, so without this
   every chat prompt would share the feature `user`). If nothing is left, all tokens
   are used; if there are no letters or digits at all, raw character trigrams are used.
3. For each remaining word `w`, in order: add the feature `w:<w>` with weight 1.0, and
   each character trigram of `<w>` (for example `<ca`, `cap`, ..., `al>`) with weight 0.5.
4. Signed feature hashing: FNV-1a 64-bit of the feature; index = hash mod DIM,
   sign = top bit of the hash.
5. L2-normalise. (A text whose features cancel out falls back to a fixed unit vector,
   so a zero vector is never returned.)

Features are added in a fixed order, so the same text always gives a bit-identical
vector.

### What the scores look like (from `go test -v`, DIM 256)

| Text A | Text B | Cosine |
|---|---|---|
| `user: what is the capital city of france?` | `user: tell me the capital city of france.` | 0.8783 |
| `user: what is the capital city of france?` | `user: how do i bake sourdough bread at home?` | -0.0321 |
| `How do I reset my router password?` | `How can I reset the password of my router?` | 1.0000 (same content words, order ignored) |
| `Summarise the quarterly sales report` | `Summarize the sales report for the quarter` | 0.6984 (a spelling variant and an extra word drop it below 0.8) |
| `Explain how HNSW indexes work in pgvector` | `How does an HNSW index work in pgvector?` | 0.7440 |

The last two rows show the limitation: a real embedding model would place those
pairs close together, this one does not. Small collisions in 256 dimensions also
add noise of roughly ±0.1 between unrelated texts.

## Tests

`mock_test.go` covers determinism (bit-identical vectors, across instances and
dimensions), L2 normalisation (including stopword-only, punctuation-only, empty and
non-Latin text), that paraphrases sharing words score higher than unrelated text,
the exact similarities the e2e run relies on, the chat answer rules, knowledge files
(matching, precedence, validation, and the answers of `docker/mock-knowledge.json`),
streaming, auth, input validation, base64 encoding, counters, reset and the request log.
