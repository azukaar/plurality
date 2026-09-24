package main

import (
	"time"
	"runtime"
	"fmt"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"

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
	search.InitEmbedWorker()
	defer db.CloseAllUserDBs()
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

	// Stall watchdog: if no SQLite write has completed for 90s the server is
	// wedged (a goroutine holding the single connection or a mutex). Dump all
	// goroutine stacks to the persistent volume so the exact blocker is
	// visible in the post-mortem. Diagnostic only; does not change behavior.
	go func() {
		for {
			time.Sleep(30 * time.Second)
			if db.SinceDBActivity() > 90*time.Second {
				os.MkdirAll("/app/users-data/sam/crashwatch", 0o755)
				path := fmt.Sprintf("/app/users-data/sam/crashwatch/stall_%d.txt", time.Now().Unix())
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				os.WriteFile(path, buf[:n], 0o644)
				log.Printf("STALL WATCHDOG: no DB write for %v; dumped goroutines to %s", db.SinceDBActivity(), path)
			}
		}
	}()

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

	// Global panic recovery: net/http does NOT recover handler panics — a
	// panic in any request handler (e.g. while processing a huge
	// conversation at "finished workflow") would terminate the entire
	// process and restart the container. Wrap the router so a panic is
	// caught, logged with its stack, and returned as a clean 500 instead
	// of crashing the server.
	recoveryMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("PANIC RECOVERED in %s %s: %v\n%s",
						r.Method, r.URL.Path, rec, debug.Stack())
					// Flusher may already have written SSE frames; we can't
					// change the status then. Best effort only.
					w.Header().Set("Content-Type", "application/json")
					http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}

	http.Handle("/", recoveryMiddleware(corsMiddleware(r)))

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

	// Generate the embedding (HTTP to LiteLLM) OUTSIDE the SQLite write lock —
	// see search.Search: a stuck embedding used to hold LockDBWrite for its
	// whole round-trip, blocking every writer and crashing the server.
	var queryVec []float32
	if ai.LiteLLMBaseURL != "" {
		queryVec, _ = search.GenerateEmbedding(ai.LiteLLMBaseURL, query)
	}
	db.LockDBWrite()
	results, err := search.SearchWithVec(userDB, query, queryVec, limit)
	db.UnlockDBWrite()
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}
