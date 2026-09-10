# AI Knowledge Assistant (Simple Version)

Go + Gin backend, PostgreSQL, JWT auth, OpenAI API for Q&A/summarization, plain React frontend (CDN, no build step).

Matches this scope:
- JWT authentication
- Document upload/storage per user
- AI API integration to answer questions and summarize documents
- React dashboard for managing documents and interacting with the assistant

## What's different from a "RAG" build
No pgvector, no embeddings, no multi-tenancy/workspaces. Documents belong directly to a user. Questions are answered either against one selected document or the user's 5 most recent documents, sent as plain text context to the LLM. This is simpler and cheaper to run, but doesn't scale to a large document library the way vector search does — worth mentioning if asked in an interview.

## Setup

### 1. Database
```bash
createdb aidocassistant
psql aidocassistant < backend/schema.sql
```

### 2. Backend
```bash
cd backend
go mod tidy
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/aidocassistant"
export OPENAI_API_KEY="sk-..."
export JWT_SECRET="something-random"
go run main.go
```
Runs on `:8080`.

### 3. Frontend
Open `frontend/index.html` directly in a browser, or serve it:
```bash
cd frontend
python3 -m http.server 3000
```

## API
- `POST /signup` `{email, password}` → `{token}`
- `POST /login` `{email, password}` → `{token}`
- `GET /api/documents` → list user's documents
- `POST /api/documents` `{title, content}` → upload
- `POST /api/documents/:id/summarize` → `{summary}`
- `POST /api/ask` `{question, doc_id?}` → `{answer}` (doc_id optional — omit to search across recent docs)

## Known simplifications
- No chunking/embeddings — full document text goes straight into the LLM prompt, so very long documents may exceed context limits.
- "Across all documents" only looks at the 5 most recent, not a real search.
- No PDF parsing — paste text only.
- CORS wide open, fine for a demo.
