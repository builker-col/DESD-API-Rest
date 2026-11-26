package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"desd-api/internal/config"
	"desd-api/internal/db"
	"desd-api/internal/export"
	"desd-api/internal/ingest"
	"desd-api/internal/queue"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	cfg   config.Config
	store *db.Store
	pub   *queue.Publisher
}

func NewServer(cfg config.Config, store *db.Store, pub *queue.Publisher) *Server {
	return &Server{cfg: cfg, store: store, pub: pub}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Healthcheck
	mux.HandleFunc("GET /health", s.handleHealth)

	// Auth
	mux.HandleFunc("POST /api/v1/auth", s.handleAuth)

	// User
	mux.HandleFunc("POST /api/v1/user", s.handleUserCreate)
	mux.HandleFunc("GET /api/v1/user", s.requireAuth(s.handleUserGet))
	mux.HandleFunc("PUT /api/v1/user", s.requireAuth(s.handleUserUpdate))
	mux.HandleFunc("DELETE /api/v1/user", s.requireAuth(s.handleUserDelete))

	// Worker & Duplicate
	mux.HandleFunc("POST /api/v1/worker", s.requireAuth(s.handleWorkerUpload))
	mux.HandleFunc("POST /api/v1/duplicate", s.requireAuth(s.handleDuplicateUpload))

	// Tasks
	mux.HandleFunc("GET /api/v1/task", s.requireAuth(s.handleTaskList))
	mux.HandleFunc("GET /api/v1/task/{task_id}", s.requireAuth(s.handleTaskGet))
	mux.HandleFunc("DELETE /api/v1/task/{task_id}", s.requireAuth(s.handleTaskDelete))

	// Streaming CSV Export
	mux.HandleFunc("GET /api/v1/export/{task_id}", s.requireAuth(s.handleExportCSV))

	// Admin: Users
	mux.HandleFunc("GET /api/v1/admin/user", s.requireAdmin(s.handleAdminUserList))
	mux.HandleFunc("GET /api/v1/admin/user/{user_id}", s.requireAdmin(s.handleAdminUserGet))
	mux.HandleFunc("POST /api/v1/admin/user", s.requireAdmin(s.handleAdminUserCreate))
	mux.HandleFunc("PUT /api/v1/admin/user/{user_id}", s.requireAdmin(s.handleAdminUserUpdate))
	mux.HandleFunc("DELETE /api/v1/admin/user/{user_id}", s.requireAdmin(s.handleAdminUserDelete))

	// Admin: Tasks
	mux.HandleFunc("GET /api/v1/admin/task", s.requireAdmin(s.handleAdminTaskList))
	mux.HandleFunc("GET /api/v1/admin/task/{task_id}", s.requireAdmin(s.handleAdminTaskGet))
	mux.HandleFunc("DELETE /api/v1/admin/task/{task_id}", s.requireAdmin(s.handleAdminTaskDelete))

	// Admin: Allowed IPs
	mux.HandleFunc("GET /api/v1/admin/allowed_ips", s.requireAdmin(s.handleAdminIPList))
	mux.HandleFunc("GET /api/v1/admin/allowed_ips/{allowed_ip_id}", s.requireAdmin(s.handleAdminIPGet))
	mux.HandleFunc("POST /api/v1/admin/allowed_ips", s.requireAdmin(s.handleAdminIPCreate))
	mux.HandleFunc("PUT /api/v1/admin/allowed_ips/{allowed_ip_id}", s.requireAdmin(s.handleAdminIPUpdate))
	mux.HandleFunc("DELETE /api/v1/admin/allowed_ips/{allowed_ip_id}", s.requireAdmin(s.handleAdminIPDelete))

	return s.withCORSAndIP(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	pgOK := s.store.Ping(ctx) == nil
	rmqOK := s.pub.IsHealthy()

	status := http.StatusOK
	overall := "ok"
	if !pgOK || !rmqOK {
		status = http.StatusServiceUnavailable
		overall = "degraded"
	}
	writeJSON(w, status, map[string]any{
		"status":   overall,
		"postgres": pgOK,
		"rabbitmq": rmqOK,
	})
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "Username or password not provided")
		return
	}

	user, err := s.store.GetUserByUsername(r.Context(), body.Username)
	if err != nil || user == nil {
		writeJSONError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(body.Password)); err != nil {
		writeJSONError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}

	token, err := generateJWT(s.cfg.JWTSecretKey, user.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to generate token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": token})
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  *bool  `json:"is_admin"`
		IsActive *bool  `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "Username and password are required")
		return
	}
	if body.IsAdmin != nil && *body.IsAdmin {
		writeJSONError(w, http.StatusBadRequest, "Permission denied.")
		return
	}
	existing, _ := s.store.GetUserByUsername(r.Context(), body.Username)
	if existing != nil {
		writeJSONError(w, http.StatusBadRequest, "Username already exists.")
		return
	}
	active := true
	if body.IsActive != nil {
		active = *body.IsActive
	}
	user, err := s.store.CreateUser(r.Context(), body.Username, body.Password, false, active)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleUserGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r))
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
		IsAdmin  *bool   `json:"is_admin"`
		IsActive *bool   `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}
	if body.IsAdmin != nil {
		writeJSONError(w, http.StatusBadRequest, "Permission denied.")
		return
	}
	updated, err := s.store.UpdateUser(r.Context(), currentUser(r).ID, body.Username, body.Password, nil, body.IsActive)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	ok, err := s.store.DeleteUser(r.Context(), currentUser(r).ID)
	if err != nil || !ok {
		writeJSONError(w, http.StatusNotFound, "User not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) parseStreamingMultipart(r *http.Request) (map[string]string, []ingest.StagedInput, string, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, nil, "", err
	}
	uploadDir, err := os.MkdirTemp(s.cfg.SharedDataDir, "__upload_*")
	if err != nil {
		return nil, nil, "", err
	}

	fields := make(map[string]string)
	var inputs []ingest.StagedInput
	idx := 0

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(uploadDir)
			return nil, nil, "", err
		}

		formName := part.FormName()
		fileName := part.FileName()
		if fileName == "" {
			valBytes, _ := io.ReadAll(io.LimitReader(part, 64*1024))
			fields[formName] = strings.TrimSpace(string(valBytes))
			_ = part.Close()
			continue
		}

		if formName == "files" {
			idx++
			cleanName := filepath.Base(fileName)
			dstPath := filepath.Join(uploadDir, fmt.Sprintf("%d_%s", idx, cleanName))
			outF, createErr := os.Create(dstPath)
			if createErr != nil {
				_ = part.Close()
				_ = os.RemoveAll(uploadDir)
				return nil, nil, "", createErr
			}
			_, cpErr := io.Copy(outF, part)
			_ = outF.Close()
			_ = part.Close()
			if cpErr != nil {
				_ = os.RemoveAll(uploadDir)
				return nil, nil, "", cpErr
			}
			inputs = append(inputs, ingest.StagedInput{Index: idx, Path: dstPath, Filename: cleanName})
		} else {
			_ = part.Close()
		}
	}

	return fields, inputs, uploadDir, nil
}

func (s *Server) handleWorkerUpload(w http.ResponseWriter, r *http.Request) {
	fields, inputs, uploadDir, err := s.parseStreamingMultipart(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid multipart upload")
		return
	}
	defer os.RemoveAll(uploadDir)

	if len(inputs) == 0 {
		writeJSONError(w, http.StatusBadRequest, "No file part")
		return
	}

	rawModels := fields["models"]
	if rawModels == "" {
		writeJSONError(w, http.StatusBadRequest, "No models provided")
		return
	}
	models := ingest.UniqueModels(rawModels)
	var invalid []string
	for _, m := range models {
		if !ingest.ValidWorkerModels[m] {
			invalid = append(invalid, m)
		}
	}
	if len(models) == 0 || len(invalid) > 0 {
		writeJSONError(w, http.StatusBadRequest, "Invalid models: "+strings.Join(invalid, ", "))
		return
	}

	taskID := strings.TrimSpace(fields["task_id"])
	if taskID != "" {
		exists, err := s.store.TaskExists(r.Context(), taskID)
		if err == nil && exists {
			writeJSONError(w, http.StatusConflict, "Task ID already exists")
			return
		}
	} else {
		taskID, err = s.store.AllocateUniqueTaskID(r.Context(), "")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "Failed to allocate task_id")
			return
		}
	}

	resultID, err := s.store.CreateTask(r.Context(), taskID, currentUser(r).ID, len(inputs), strings.Join(models, ","))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	validFiles, totalPages := ingest.FilesStreaming(r.Context(), s.cfg, s.pub, taskID, models, false, inputs, nil)
	if validFiles == 0 || totalPages == 0 {
		_ = s.store.SetTaskStatus(r.Context(), resultID, "FAILED", 0)
		writeJSONError(w, http.StatusBadRequest, "No valid pages could be extracted")
		return
	}

	if _, err := s.store.FinalizeIngestion(r.Context(), resultID, validFiles, totalPages*len(models)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"task_id": taskID})
}

func (s *Server) handleDuplicateUpload(w http.ResponseWriter, r *http.Request) {
	fields, inputs, uploadDir, err := s.parseStreamingMultipart(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid multipart upload")
		return
	}
	defer os.RemoveAll(uploadDir)

	if len(inputs) == 0 {
		writeJSONError(w, http.StatusBadRequest, "No file part")
		return
	}

	taskID := strings.TrimSpace(fields["task_id"])
	if taskID != "" {
		exists, err := s.store.TaskExists(r.Context(), taskID)
		if err == nil && exists {
			writeJSONError(w, http.StatusConflict, "Task ID already exists")
			return
		}
	} else {
		taskID, err = s.store.AllocateUniqueTaskID(r.Context(), "")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "Failed to allocate task_id")
			return
		}
	}

	resultID, err := s.store.CreateTask(r.Context(), taskID, currentUser(r).ID, len(inputs), "dude")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	validFiles, _ := ingest.FilesStreaming(r.Context(), s.cfg, s.pub, taskID, []string{"dude"}, true, inputs, nil)
	if validFiles == 0 {
		_ = s.store.SetTaskStatus(r.Context(), resultID, "FAILED", 0)
		writeJSONError(w, http.StatusBadRequest, "No valid PDF/TIFF documents provided")
		return
	}

	taskDir := filepath.Join(s.cfg.SharedDataDir, taskID)
	if err := s.pub.PublishJSON(r.Context(), s.cfg.DuplicateQueue, ingest.DuplicateTaskMessage{
		FolderPath: taskDir,
		TaskID:     taskID,
	}); err != nil {
		_ = s.store.SetTaskStatus(r.Context(), resultID, "FAILED", validFiles)
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.SetTaskStatus(r.Context(), resultID, "PENDING", validFiles)

	writeJSON(w, http.StatusAccepted, map[string]string{"task_id": taskID})
}

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	q := r.URL.Query()
	results, err := s.store.ListTasks(r.Context(), &uid, q.Get("status"), q.Get("order_type"), q.Get("order_by"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_results": len(results),
		"results":       results,
	})
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	res, err := s.store.GetTaskByCollectionID(r.Context(), taskID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res == nil {
		writeJSONError(w, http.StatusNotFound, "Task ID not found")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTaskDelete(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	res, err := s.store.GetTaskByCollectionID(r.Context(), taskID)
	if err != nil || res == nil {
		writeJSONError(w, http.StatusNotFound, "Task ID not found")
		return
	}
	if res.UserID != currentUser(r).ID {
		writeJSONError(w, http.StatusForbidden, "Permission denied")
		return
	}
	if err := s.store.DeleteTask(r.Context(), taskID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.SharedDataDir, taskID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	formato := r.URL.Query().Get("formato")
	if formato == "" {
		formato = "normal"
	}

	fieldnames, ok, err := export.ResolveFieldnames(r.Context(), s.store, taskID, formato)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "Task ID not found")
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, taskID))
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	_ = export.StreamCSV(r.Context(), s.store, w, taskID, formato, fieldnames)
}

// --- Admin ---

func (s *Server) handleAdminUserList(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) handleAdminUserGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("user_id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid user_id")
		return
	}
	u, err := s.store.GetUserByID(r.Context(), id)
	if err != nil || u == nil {
		writeJSONError(w, http.StatusNotFound, "User not found")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"is_admin"`
		IsActive *bool  `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "Username and password are required")
		return
	}
	active := true
	if body.IsActive != nil {
		active = *body.IsActive
	}
	u, err := s.store.CreateUser(r.Context(), body.Username, body.Password, body.IsAdmin, active)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) handleAdminUserUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("user_id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid user_id")
		return
	}
	var body struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
		IsAdmin  *bool   `json:"is_admin"`
		IsActive *bool   `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}
	u, err := s.store.UpdateUser(r.Context(), id, body.Username, body.Password, body.IsAdmin, body.IsActive)
	if err != nil || u == nil {
		writeJSONError(w, http.StatusNotFound, "User not found")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleAdminUserDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("user_id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid user_id")
		return
	}
	ok, err := s.store.DeleteUser(r.Context(), id)
	if err != nil || !ok {
		writeJSONError(w, http.StatusNotFound, "User not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminTaskList(w http.ResponseWriter, r *http.Request) {
	results, err := s.store.ListTasks(r.Context(), nil, "", "created_at", "desc")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *Server) handleAdminTaskGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	res, err := s.store.GetTaskByCollectionID(r.Context(), taskID)
	if err != nil || res == nil {
		writeJSONError(w, http.StatusNotFound, "Task ID not found")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAdminTaskDelete(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	res, err := s.store.GetTaskByCollectionID(r.Context(), taskID)
	if err != nil || res == nil {
		writeJSONError(w, http.StatusNotFound, "Task ID not found")
		return
	}
	if err := s.store.DeleteTask(r.Context(), taskID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.SharedDataDir, taskID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminIPList(w http.ResponseWriter, r *http.Request) {
	ips, err := s.store.ListAllowedIPs(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ips)
}

func (s *Server) handleAdminIPGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("allowed_ip_id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid allowed_ip_id")
		return
	}
	ip, err := s.store.GetAllowedIP(r.Context(), id)
	if err != nil || ip == nil {
		writeJSONError(w, http.StatusNotFound, "Allowed IP not found")
		return
	}
	writeJSON(w, http.StatusOK, ip)
}

func (s *Server) handleAdminIPCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP       string `json:"ip"`
		IsActive *bool  `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.IP) == "" {
		writeJSONError(w, http.StatusBadRequest, "IP is required")
		return
	}
	active := true
	if body.IsActive != nil {
		active = *body.IsActive
	}
	a, err := s.store.CreateAllowedIP(r.Context(), strings.TrimSpace(body.IP), active)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleAdminIPUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("allowed_ip_id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid allowed_ip_id")
		return
	}
	var body struct {
		IP       *string `json:"ip"`
		IsActive *bool   `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}
	a, err := s.store.UpdateAllowedIP(r.Context(), id, body.IP, body.IsActive)
	if err != nil || a == nil {
		writeJSONError(w, http.StatusNotFound, "Allowed IP not found")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleAdminIPDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("allowed_ip_id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid allowed_ip_id")
		return
	}
	ok, err := s.store.DeleteAllowedIP(r.Context(), id)
	if err != nil || !ok {
		writeJSONError(w, http.StatusNotFound, "Allowed IP not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
