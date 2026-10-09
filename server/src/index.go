package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/cron"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/jobs"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/search"
	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/startup"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/user"
	"github.com/azukaar/plurality/src/utils"
	"github.com/azukaar/plurality/src/version"
	"github.com/azukaar/plurality/src/webhook"
	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	// CLI subcommands (adduser / removeuser / listusers) short-circuit before
	// the server starts.
	if handled, code := auth.HandleCLI(os.Args[1:]); handled {
		os.Exit(code)
	}

	auth.Init()
	startup.Run()
	db.InitSQLite()
	defer db.CloseAllUserDBs()
	// Single serialized embedding worker — without it, every message push
	// spawns a goroutine that queues on the SetMaxOpenConns(1) SQLite pool
	// and wedges the backend under load (see search/search.go).
	search.InitEmbedWorker()
	storage.Init()

	// Load global server-side skills from data/skills/ and per-user skills from
	// users-data/{userID}/skills/. The retrieve_server_skill tool is registered
	// unconditionally — GetRequests only surfaces it to users that actually have
	// skills (skills.HasAny(userID)), and it 404s gracefully otherwise.
	skills.Init()
	ai_tools.RegisterRetrieveServerSkill()

	// Load mini app presets from data/presets/
	miniapps.LoadBuiltins()

	// Start MCP servers configured in data/mcp.json
	mcp.Init()
	defer mcp.Shutdown()

	// Initialize LiteLLM proxy for AI provider routing
	if err := ai.InitLiteLLM(); err != nil {
		log.Printf("[main] WARNING: LiteLLM proxy not available: %v", err)
		log.Printf("[main] Set LITELLM_URL env var or run litellm/setup.sh to enable AI features")
	} else {
		// Pull the model registry from litellm. The proxy is the single source of
		// truth for which models exist and what they support.
		if err := ai.InitModels(context.Background()); err != nil {
			log.Printf("[main] WARNING: failed to load models from litellm: %v", err)
		}
	}
	defer ai.ShutdownLiteLLM()

	// Pass LiteLLM URL to packages that talk to the proxy directly.
	db.LiteLLMBaseURL = ai.LiteLLMBaseURL
	ai_tools.LiteLLMBaseURL = ai.LiteLLMBaseURL

	// Diagnostic watchdog: periodically snapshot ALL goroutine stacks to the
	// persistent volume so a wedge (goroutine holding a SQLite connection /
	// mutex forever) is observable after the fact. RATE-LIMITED: dumps at most
	// once per 10 minutes and prunes to the newest 20 files. Without the cap
	// this flooded the volume with ~1MB dumps every 30-60s (833MB in 48h) and
	// masked real incidents behind a wall of noise. Diagnostic only; no
	// behavior change. Enabled only when DEBUG=1 is set — in production the
	// ~1MB goroutine dumps are pure overhead/noise.
	if os.Getenv("DEBUG") == "1" {
		var lastDump atomic.Int64
		go func() {
			for {
				time.Sleep(5 * time.Minute)
				now := time.Now().Unix()
				if now-lastDump.Load() < 10*60 {
					continue
				}
				lastDump.Store(now)
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				base := os.Getenv("USER_DATA_STORAGE")
				if base == "" {
					base = "users-data"
				}
				dir := filepath.Join(base, "crashwatch")
				os.MkdirAll(dir, 0o755)
				path := filepath.Join(dir, fmt.Sprintf("goroutines_%d.txt", time.Now().Unix()))
				os.WriteFile(path, buf[:n], 0o644)
				// Prune: keep only the 20 newest dumps.
				entries, _ := filepath.Glob(filepath.Join(dir, "goroutines_*.txt"))
				if len(entries) > 20 {
					// Entries are timestamped; sort ascending and drop the oldest.
					sort.Strings(entries)
					for _, old := range entries[:len(entries)-20] {
						os.Remove(old)
					}
				}
				log.Printf("[watchdog] wrote goroutine dump to %s", path)
			}
		}()
	}

	// CRON scheduler: rebuild every user's jobs from disk and start the loop.
	cron.Init()
	ai_tools.RegisterTool(cron.Tool)
	defer cron.Shutdown()

	// Periodic server update check (logs when a newer release is published).
	version.Init()

	// Webhooks: rebuild the in-memory ID -> userID index from disk.
	webhook.Init()
	ai_tools.RegisterTool(webhook.Tool)
	defer webhook.Shutdown()

	// conversations__ bundle: create_conversation + parallel_sub_agent +
	// parallel_sub_agent_background_manage. Live in jobs so they can reach
	// ai.* (icon generation, RequestRegistry) and jobs.RunSubAgent without
	// circling back through ai_tools.
	ai_tools.RegisterTool(jobs.CreateConversationTool)
	ai_tools.RegisterTool(jobs.ParallelSubAgentTool)
	ai_tools.RegisterTool(jobs.ManageSubAgentTool)

	utils.Log("[main] Starting server on :8090")

	r := mux.NewRouter()

	// --- Auth (no middleware on login/methods; everything else requires JWT) ---
	r.HandleFunc("/auth/methods", auth.HandleAuthMethods).Methods("GET", "OPTIONS")
	r.HandleFunc("/auth/login", auth.HandleLogin).Methods("POST", "OPTIONS")
	r.HandleFunc("/auth/openid/exchange", auth.HandleOIDCExchange).Methods("POST", "OPTIONS")
	r.HandleFunc("/auth/me", auth.AuthMiddleware(auth.HandleMe)).Methods("GET", "OPTIONS")
	r.HandleFunc("/auth/logout", auth.AuthMiddleware(auth.HandleLogout)).Methods("POST", "OPTIONS")
	r.HandleFunc("/auth/change-password", auth.AuthMiddleware(auth.HandleChangePassword)).Methods("POST", "OPTIONS")
	r.HandleFunc("/shortcuts/{name}", auth.AuthMiddleware(auth.HandleSetShortcut)).Methods("PUT", "OPTIONS")

	// OpenAI-compatible API (stateless, for generic clients)
	r.HandleFunc("/v1/chat/completions", auth.AuthMiddleware(ai.HandleOpenAIChatCompletion)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/models", auth.AuthMiddleware(ai.HandleOpenAIListModels)).Methods("GET", "OPTIONS")
	r.HandleFunc("/v1/tools", auth.AuthMiddleware(ai.HandleListServerTools)).Methods("GET", "OPTIONS")

	// Plurality rich models endpoint: OpenAI-list-compatible with embedded presets + function metadata.
	r.HandleFunc("/models", auth.AuthMiddleware(ai.HandleListModels)).Methods("GET", "OPTIONS")

	// Plurality chat (stateful, with conversation tracking and server-side tool loop)
	r.HandleFunc("/chat", auth.AuthMiddleware(ai.HandleChat)).Methods("POST", "OPTIONS")
	r.HandleFunc("/chat/stream/{id}", auth.AuthMiddleware(ai.HandleStreamReconnect)).Methods("GET", "OPTIONS")
	r.HandleFunc("/chat/cancel/{id}", auth.AuthMiddleware(ai.HandleCancel)).Methods("POST", "OPTIONS")
	r.HandleFunc("/chat/approve/{id}", auth.AuthMiddleware(ai.HandleApprove)).Methods("POST", "OPTIONS")
	r.HandleFunc("/status/stream", auth.AuthMiddleware(ai.HandleStatusStream)).Methods("GET", "OPTIONS")

	// Conversations
	r.HandleFunc("/conversations", auth.AuthMiddleware(ai.API_ListConversation)).Methods("GET", "OPTIONS")
	r.HandleFunc("/conversation/{id}", auth.AuthMiddleware(ai.API_HandleConversation)).Methods("GET", "PUT", "DELETE", "OPTIONS")
	r.HandleFunc("/set-conversation-folder/{id}", auth.AuthMiddleware(ai.API_UpdateConversationFolder)).Methods("POST", "OPTIONS")
	r.HandleFunc("/rename-conversation/{id}", auth.AuthMiddleware(ai.API_UpdateConversationTitle)).Methods("POST", "OPTIONS")
	r.HandleFunc("/generate-title/{id}", auth.AuthMiddleware(ai.API_HandleTitleGeneration)).Methods("GET", "OPTIONS")
	r.HandleFunc("/transcribe", auth.AuthMiddleware(ai.HandleTranscribe)).Methods("POST", "OPTIONS")
	r.HandleFunc("/generate-audio", auth.AuthMiddleware(ai.HandleGenerateAudio)).Methods("POST", "OPTIONS")
	r.HandleFunc("/delete-user", auth.AuthMiddleware(user.API_DeleteUser)).Methods("DELETE", "OPTIONS")
	r.HandleFunc("/user/memory", auth.AuthMiddleware(user.API_GetMemory)).Methods("GET", "OPTIONS")
	r.HandleFunc("/user/memory", auth.AuthMiddleware(user.API_UpdateMemory)).Methods("PUT", "OPTIONS")

	// CRON scheduled prompts
	r.HandleFunc("/crons", auth.AuthMiddleware(cron.API_ListCrons)).Methods("GET", "OPTIONS")
	r.HandleFunc("/crons", auth.AuthMiddleware(cron.API_CreateCron)).Methods("POST", "OPTIONS")
	r.HandleFunc("/crons/{id}", auth.AuthMiddleware(cron.API_UpdateCron)).Methods("PUT", "OPTIONS")
	r.HandleFunc("/crons/{id}", auth.AuthMiddleware(cron.API_DeleteCron)).Methods("DELETE", "OPTIONS")
	r.HandleFunc("/crons/{id}/run", auth.AuthMiddleware(cron.API_RunCron)).Methods("POST", "OPTIONS")

	// Webhooks: authenticated CRUD + public trigger endpoint.
	r.HandleFunc("/webhooks", auth.AuthMiddleware(webhook.API_ListWebhooks)).Methods("GET", "OPTIONS")
	r.HandleFunc("/webhooks", auth.AuthMiddleware(webhook.API_CreateWebhook)).Methods("POST", "OPTIONS")
	r.HandleFunc("/webhooks/{id}", auth.AuthMiddleware(webhook.API_UpdateWebhook)).Methods("PUT", "OPTIONS")
	r.HandleFunc("/webhooks/{id}", auth.AuthMiddleware(webhook.API_DeleteWebhook)).Methods("DELETE", "OPTIONS")
	r.HandleFunc("/webhooks/{id}/rotate", auth.AuthMiddleware(webhook.API_RotateWebhookToken)).Methods("POST", "OPTIONS")

	// PUBLIC trigger endpoint — token in URL or header IS the auth. No AuthMiddleware.
	r.HandleFunc("/webhook/{id}", webhook.API_TriggerWebhook).Methods("GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS")

	// Search
	r.HandleFunc("/search", auth.AuthMiddleware(handleSearch)).Methods("GET", "OPTIONS")

	// Mini apps (file-backed)
	r.HandleFunc("/miniapps", auth.AuthMiddleware(miniapps.API_ListMiniApps)).Methods("GET", "OPTIONS")
	r.HandleFunc("/miniapps", auth.AuthMiddleware(miniapps.API_CreateMiniApp)).Methods("POST", "OPTIONS")
	r.HandleFunc("/miniapps/pinned", auth.AuthMiddleware(miniapps.API_GetUserPinnedMiniApps)).Methods("GET", "OPTIONS")
	r.HandleFunc("/miniapps/{id}", auth.AuthMiddleware(miniapps.API_HandleMiniApp)).Methods("GET", "DELETE", "OPTIONS")
	r.HandleFunc("/miniapps/{id}", auth.AuthMiddleware(miniapps.API_UpdateMiniApp)).Methods("PUT")
	r.HandleFunc("/miniapps/{id}/pin", auth.AuthMiddleware(miniapps.API_PinMiniApp)).Methods("POST", "OPTIONS")
	r.HandleFunc("/miniapps/{id}/unpin", auth.AuthMiddleware(miniapps.API_UnpinMiniApp)).Methods("POST", "OPTIONS")

	r.HandleFunc("/download/{file}",
		func(w http.ResponseWriter, r *http.Request) {
			vars := mux.Vars(r)
			file := vars["file"]
			if file == "windows-latest.exe" || file == "linux-latest.zip" || file == "macos-latest.dmg" {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", "attachment; filename="+file)
				http.ServeFile(w, r, filepath.Join("web", file))
			} else {
				http.NotFound(w, r)
			}
		}).Methods("GET")

	// Attachment file serving (authenticated)
	r.HandleFunc("/attachments/{userId}/{month}/{filename}", auth.AuthMiddleware(storage.ServeAttachment)).Methods("GET", "OPTIONS")
	r.HandleFunc("/upload", auth.AuthMiddleware(storage.HandleUpload)).Methods("POST", "OPTIONS")

	r.HandleFunc("/check", auth.AuthMiddleware(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		},
	)).Methods("GET", "OPTIONS")

	// Liveness probe for healthchecks / orchestrators (autoheal, k8s, etc.).
	// Returns a real 200 JSON (not the SPA catch-all) so external watchers can
	// distinguish "HTTP stack is up" from a missing route. Deliberately
	// unauthenticated — healthcheckers have no session cookie.
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}).Methods("GET", "OPTIONS")

	// /static folder as SPA
	exec, _ := os.Executable()
	pwd := filepath.Dir(exec)
	p := filepath.Join(pwd, "web")
	r.PathPrefix("/").Handler(utils.SPAHandler(p))

	// CORS middleware wrapper for the entire router
	corsMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}

	http.Handle("/", corsMiddleware(r))

	log.Printf("Server starting on port 8090...")
	log.Fatal(http.ListenAndServe(":8090", nil))
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		utils.SendHTTPError(w, "Missing query parameter 'q'", http.StatusBadRequest)
		return
	}

	userID, ok := r.Context().Value("userID").(string)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}

	userDB, err := db.GetUserDB(userID)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	results, err := search.Search(r.Context(), userDB, ai.LiteLLMBaseURL, query, limit)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}
