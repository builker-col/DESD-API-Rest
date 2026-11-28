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

	// Excel Report (Admin & Encargado)
	mux.HandleFunc("GET /api/v1/report/excel", s.requireRoles(db.RoleAdmin, db.RoleEncargado)(s.handleReportExcel))

	// Encargado: Assigned users
	mux.HandleFunc("GET /api/v1/encargado/users", s.requireRoles(db.RoleAdmin, db.RoleEncargado)(s.handleEncargadoUsersList))

	// Admin: Users
	mux.HandleFunc("GET /api/v1/admin/user", s.requireAdmin(s.handleAdminUserList))
	mux.HandleFunc("GET /api/v1/admin/user/{user_id}", s.requireAdmin(s.handleAdminUserGet))
	mux.HandleFunc("POST /api/v1/admin/user", s.requireAdmin(s.handleAdminUserCreate))
	mux.HandleFunc("PUT /api/v1/admin/user/{user_id}", s.requireAdmin(s.handleAdminUserUpdate))
	mux.HandleFunc("DELETE /api/v1/admin/user/{user_id}", s.requireAdmin(s.handleAdminUserDelete))

	// Admin: Encargado <-> Users assignments
	mux.HandleFunc("GET /api/v1/admin/encargado/{encargado_id}/users", s.requireAdmin(s.handleAdminEncargadoUsersList))
	mux.HandleFunc("PUT /api/v1/admin/encargado/{encargado_id}/users", s.requireAdmin(s.handleAdminEncargadoUsersSet))
	mux.HandleFunc("POST /api/v1/admin/encargado/{encargado_id}/users/{user_id}", s.requireAdmin(s.handleAdminEncargadoUserAdd))
	mux.HandleFunc("DELETE /api/v1/admin/encargado/{encargado_id}/users/{user_id}", s.requireAdmin(s.handleAdminEncargadoUserRemove))

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
	if !user.IsActive {
		writeJSONError(w, http.StatusForbidden, "User account is inactive")
		return
	}

	token, err := generateJWT(s.cfg.JWTSecretKey, user.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to generate token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"role":         user.EffectiveRole(),
		"user":         user,
	})
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string  `json:"username"`
		Password string  `json:"password"`
		Role     *string `json:"role"`
		IsAdmin  *bool   `json:"is_admin"`
		IsActive *bool   `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "Username and password are required")
		return
	}
	if body.IsAdmin != nil && *body.IsAdmin {
		writeJSONError(w, http.StatusBadRequest, "Permission denied.")
		return
	}
	if body.Role != nil && strings.ToLower(strings.TrimSpace(*body.Role)) != "" && strings.ToLower(strings.TrimSpace(*body.Role)) != db.RoleUser {
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
	user, err := s.store.CreateUser(r.Context(), body.Username, body.Password, db.RoleUser, active)
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
		Role     *string `json:"role"`
		IsAdmin  *bool   `json:"is_admin"`
		IsActive *bool   `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}
	if body.IsAdmin != nil || body.Role != nil {
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
			rawPath := strings.TrimSpace(fileName)
			cleanName := filepath.Base(rawPath)
			dstPath := filepath.Join(uploadDir, fmt.Sprintf("%d_%s", idx, cleanName))
			outF, createErr := os.Create(dstPath)
			if createErr != nil {
				_ = part.Close()
				_ = os.RemoveAll(uploadDir)
				return nil, nil, "", createErr
			}
			written, cpErr := io.Copy(outF, part)
			_ = outF.Close()
			_ = part.Close()
			if cpErr != nil {
				_ = os.RemoveAll(uploadDir)
				return nil, nil, "", cpErr
			}
			inputs = append(inputs, ingest.StagedInput{
				Index:      idx,
				Path:       dstPath,
				Filename:   cleanName,
				SourcePath: rawPath,
				SizeBytes:  written,
			})
		} else {
			_ = part.Close()
		}
	}

	if prefix := strings.TrimRight(strings.TrimSpace(fields["source_path"]), "/\\"); prefix != "" {
		for i := range inputs {
			if inputs[i].SourcePath == "" || inputs[i].SourcePath == inputs[i].Filename {
				inputs[i].SourcePath = prefix + "/" + inputs[i].Filename
			}
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

	validFiles, totalPages, ingestedFiles := ingest.FilesStreaming(r.Context(), s.cfg, s.pub, taskID, models, false, inputs, nil)
	if validFiles == 0 || totalPages == 0 {
		_ = s.store.SetTaskStatus(r.Context(), resultID, "FAILED", 0)
		writeJSONError(w, http.StatusBadRequest, "No valid pages could be extracted")
		return
	}

	if len(ingestedFiles) > 0 {
		dbFiles := make([]db.ResultFile, 0, len(ingestedFiles))
		for _, f := range ingestedFiles {
			dbFiles = append(dbFiles, db.ResultFile{
				Filename:  f.Filename,
				FilePath:  f.FilePath,
				Source:    "api",
				Pages:     f.Pages,
				SizeBytes: f.SizeBytes,
			})
		}
		_ = s.store.InsertResultFiles(r.Context(), resultID, taskID, dbFiles)
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

	validFiles, _, ingestedFiles := ingest.FilesStreaming(r.Context(), s.cfg, s.pub, taskID, []string{"dude"}, true, inputs, nil)
	if validFiles == 0 {
		_ = s.store.SetTaskStatus(r.Context(), resultID, "FAILED", 0)
		writeJSONError(w, http.StatusBadRequest, "No valid PDF/TIFF documents provided")
		return
	}

	if len(ingestedFiles) > 0 {
		dbFiles := make([]db.ResultFile, 0, len(ingestedFiles))
		for _, f := range ingestedFiles {
			dbFiles = append(dbFiles, db.ResultFile{
				Filename:  f.Filename,
				FilePath:  f.FilePath,
				Source:    "api",
				Pages:     f.Pages,
				SizeBytes: f.SizeBytes,
			})
		}
		_ = s.store.InsertResultFiles(r.Context(), resultID, taskID, dbFiles)
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

func resolveTaskScope(
	viewer *db.User,
	requestedUserID *int,
	assignedUserIDs []int,
) ([]int, int, string) {
	if viewer == nil {
		return nil, http.StatusUnauthorized, "Unauthorized"
	}

	switch viewer.EffectiveRole() {
	case db.RoleAdmin:
		if requestedUserID != nil {
			return []int{*requestedUserID}, 0, ""
		}
		return nil, 0, ""

	case db.RoleEncargado:
		allowedSet := make(map[int]bool, len(assignedUserIDs)+1)
		allowed := make([]int, 0, len(assignedUserIDs)+1)
		allowedSet[viewer.ID] = true
		allowed = append(allowed, viewer.ID)
		for _, id := range assignedUserIDs {
			if !allowedSet[id] {
				allowedSet[id] = true
				allowed = append(allowed, id)
			}
		}
		if requestedUserID != nil {
			if !allowedSet[*requestedUserID] {
				return nil, http.StatusForbidden, "Permission denied"
			}
			return []int{*requestedUserID}, 0, ""
		}
		return allowed, 0, ""

	default:
		if requestedUserID != nil && *requestedUserID != viewer.ID {
			return nil, http.StatusForbidden, "Permission denied"
		}
		return []int{viewer.ID}, 0, ""
	}
}

func resolveRoleForCreate(rawRole *string, isAdmin bool) (string, error) {
	if rawRole != nil && strings.TrimSpace(*rawRole) != "" {
		r := strings.ToLower(strings.TrimSpace(*rawRole))
		if !db.IsValidRole(r) {
			return "", fmt.Errorf("Invalid role. Allowed values: admin, encargado, user")
		}
		return r, nil
	}
	if isAdmin {
		return db.RoleAdmin, nil
	}
	return db.RoleUser, nil
}

func resolveRoleForUpdate(existing *db.User, rawRole *string, isAdmin *bool) (*string, error) {
	if rawRole != nil {
		r := strings.ToLower(strings.TrimSpace(*rawRole))
		if !db.IsValidRole(r) {
			return nil, fmt.Errorf("Invalid role. Allowed values: admin, encargado, user")
		}
		return &r, nil
	}
	if isAdmin != nil {
		if *isAdmin {
			r := db.RoleAdmin
			return &r, nil
		}
		if existing != nil && existing.IsRoleAdmin() {
			r := db.RoleUser
			return &r, nil
		}
	}
	return nil, nil
}

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	viewer := currentUser(r)
	q := r.URL.Query()

	var requestedUserID *int
	if rawUID := strings.TrimSpace(q.Get("user_id")); rawUID != "" {
		parsed, err := strconv.Atoi(rawUID)
		if err != nil || parsed <= 0 {
			writeJSONError(w, http.StatusBadRequest, "Invalid user_id")
			return
		}
		requestedUserID = &parsed
	}

	var assignedIDs []int
	if viewer.IsRoleEncargado() {
		var err error
		assignedIDs, err = s.store.GetAssignedUserIDs(r.Context(), viewer.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	allowedUserIDs, errStatus, errMsg := resolveTaskScope(viewer, requestedUserID, assignedIDs)
	if errStatus != 0 {
		writeJSONError(w, errStatus, errMsg)
		return
	}

	results, err := s.store.ListTasks(r.Context(), allowedUserIDs, q.Get("status"), q.Get("order_type"), q.Get("order_by"))
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
	allowed, err := s.store.CanUserAccessOwner(r.Context(), currentUser(r), res.UserID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusForbidden, "Permission denied")
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
	viewer := currentUser(r)
	if !viewer.IsRoleAdmin() && res.UserID != viewer.ID {
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
	res, err := s.store.GetTaskByCollectionID(r.Context(), taskID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res == nil {
		writeJSONError(w, http.StatusNotFound, "Task ID not found")
		return
	}
	allowed, err := s.store.CanUserAccessOwner(r.Context(), currentUser(r), res.UserID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusForbidden, "Permission denied")
		return
	}

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

func parseRequestedUserIDs(rawSingle, rawCSV string) ([]int, error) {
	seen := make(map[int]bool)
	var out []int
	addToken := func(tok string) error {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return nil
		}
		id, err := strconv.Atoi(tok)
		if err != nil || id <= 0 {
			return fmt.Errorf("invalid user_id: %s", tok)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
		return nil
	}
	if err := addToken(rawSingle); err != nil {
		return nil, err
	}
	if rawCSV != "" {
		for _, part := range strings.Split(rawCSV, ",") {
			if err := addToken(part); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func resolveReportUserScope(
	viewer *db.User,
	requestedUserIDs []int,
	assignedUserIDs []int,
) ([]int, int, string) {
	if viewer == nil {
		return nil, http.StatusUnauthorized, "Unauthorized"
	}

	switch viewer.EffectiveRole() {
	case db.RoleAdmin:
		if len(requestedUserIDs) > 0 {
			return requestedUserIDs, 0, ""
		}
		return nil, 0, ""

	case db.RoleEncargado:
		allowedSet := make(map[int]bool, len(assignedUserIDs)+1)
		allowed := make([]int, 0, len(assignedUserIDs)+1)
		allowedSet[viewer.ID] = true
		allowed = append(allowed, viewer.ID)
		for _, id := range assignedUserIDs {
			if !allowedSet[id] {
				allowedSet[id] = true
				allowed = append(allowed, id)
			}
		}
		if len(requestedUserIDs) > 0 {
			for _, reqID := range requestedUserIDs {
				if !allowedSet[reqID] {
					return nil, http.StatusForbidden, "Permission denied"
				}
			}
			return requestedUserIDs, 0, ""
		}
		return allowed, 0, ""

	default:
		return nil, http.StatusForbidden, "Permission denied"
	}
}

func parseDateParam(raw string, endOfDay bool) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		if endOfDay {
			t = t.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
		}
		return &t, nil
	}
	return nil, fmt.Errorf("invalid date format (expected YYYY-MM-DD or RFC3339): %s", raw)
}

func (s *Server) handleReportExcel(w http.ResponseWriter, r *http.Request) {
	viewer := currentUser(r)
	q := r.URL.Query()

	requestedIDs, err := parseRequestedUserIDs(q.Get("user_id"), q.Get("user_ids"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	fromRaw := q.Get("from")
	if fromRaw == "" {
		fromRaw = q.Get("fecha_desde")
	}
	toRaw := q.Get("to")
	if toRaw == "" {
		toRaw = q.Get("fecha_hasta")
	}

	fromTime, err := parseDateParam(fromRaw, false)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	toTime, err := parseDateParam(toRaw, true)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	var assignedIDs []int
	if viewer.IsRoleEncargado() {
		assignedIDs, err = s.store.GetAssignedUserIDs(r.Context(), viewer.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	allowedUserIDs, errStatus, errMsg := resolveReportUserScope(viewer, requestedIDs, assignedIDs)
	if errStatus != 0 {
		writeJSONError(w, errStatus, errMsg)
		return
	}

	rows, err := s.store.ListFileReportRows(r.Context(), allowedUserIDs, q.Get("status"), fromTime, toTime)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	filename := fmt.Sprintf("reporte_archivos_%s.xlsx", time.Now().Format("20060102_150405"))
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	_ = export.WriteFilesExcelReport(w, rows)
}

// --- Encargado ---

func (s *Server) handleEncargadoUsersList(w http.ResponseWriter, r *http.Request) {
	viewer := currentUser(r)
	targetEncargadoID := viewer.ID
	if viewer.IsRoleAdmin() {
		if rawID := strings.TrimSpace(r.URL.Query().Get("encargado_id")); rawID != "" {
			parsed, err := strconv.Atoi(rawID)
			if err != nil || parsed <= 0 {
				writeJSONError(w, http.StatusBadRequest, "Invalid encargado_id")
				return
			}
			targetEncargadoID = parsed
		}
	}
	users, err := s.store.ListAssignedUsers(r.Context(), targetEncargadoID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// --- Admin ---

func (s *Server) handleAdminUserList(w http.ResponseWriter, r *http.Request) {
	roleFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("role")))
	if roleFilter != "" && !db.IsValidRole(roleFilter) {
		writeJSONError(w, http.StatusBadRequest, "Invalid role filter. Allowed values: admin, encargado, user")
		return
	}
	users, err := s.store.ListUsers(r.Context(), roleFilter)
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
		Username        string  `json:"username"`
		Password        string  `json:"password"`
		Role            *string `json:"role"`
		IsAdmin         bool    `json:"is_admin"`
		IsActive        *bool   `json:"is_active"`
		AssignedUserIDs []int   `json:"assigned_user_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "Username and password are required")
		return
	}
	role, err := resolveRoleForCreate(body.Role, body.IsAdmin)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.AssignedUserIDs) > 0 && role != db.RoleEncargado {
		writeJSONError(w, http.StatusBadRequest, "assigned_user_ids can only be set for users with role 'encargado'")
		return
	}
	active := true
	if body.IsActive != nil {
		active = *body.IsActive
	}
	u, err := s.store.CreateUser(r.Context(), body.Username, body.Password, role, active)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.AssignedUserIDs) > 0 {
		assigned, err := s.store.SetAssignedUsers(r.Context(), u.ID, body.AssignedUserIDs)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		ids := make([]int, 0, len(assigned))
		for _, au := range assigned {
			ids = append(ids, au.ID)
		}
		u.AssignedUserIDs = ids
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
		Username        *string `json:"username"`
		Password        *string `json:"password"`
		Role            *string `json:"role"`
		IsAdmin         *bool   `json:"is_admin"`
		IsActive        *bool   `json:"is_active"`
		AssignedUserIDs *[]int  `json:"assigned_user_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}
	existing, err := s.store.GetUserByID(r.Context(), id)
	if err != nil || existing == nil {
		writeJSONError(w, http.StatusNotFound, "User not found")
		return
	}
	newRole, err := resolveRoleForUpdate(existing, body.Role, body.IsAdmin)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	effectiveRole := existing.EffectiveRole()
	if newRole != nil {
		effectiveRole = *newRole
	}
	if body.AssignedUserIDs != nil && effectiveRole != db.RoleEncargado {
		writeJSONError(w, http.StatusBadRequest, "assigned_user_ids can only be set for users with role 'encargado'")
		return
	}
	u, err := s.store.UpdateUser(r.Context(), id, body.Username, body.Password, newRole, body.IsActive)
	if err != nil || u == nil {
		writeJSONError(w, http.StatusBadRequest, "Failed to update user")
		return
	}
	if body.AssignedUserIDs != nil {
		assigned, err := s.store.SetAssignedUsers(r.Context(), id, *body.AssignedUserIDs)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		ids := make([]int, 0, len(assigned))
		for _, au := range assigned {
			ids = append(ids, au.ID)
		}
		u.AssignedUserIDs = ids
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

func (s *Server) requireValidEncargado(w http.ResponseWriter, r *http.Request) (*db.User, bool) {
	encID, err := strconv.Atoi(r.PathValue("encargado_id"))
	if err != nil || encID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "Invalid encargado_id")
		return nil, false
	}
	enc, err := s.store.GetUserByID(r.Context(), encID)
	if err != nil || enc == nil {
		writeJSONError(w, http.StatusNotFound, "Encargado user not found")
		return nil, false
	}
	if !enc.IsRoleEncargado() {
		writeJSONError(w, http.StatusBadRequest, "Target user does not have role 'encargado'")
		return nil, false
	}
	return enc, true
}

func (s *Server) handleAdminEncargadoUsersList(w http.ResponseWriter, r *http.Request) {
	enc, ok := s.requireValidEncargado(w, r)
	if !ok {
		return
	}
	users, err := s.store.ListAssignedUsers(r.Context(), enc.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) handleAdminEncargadoUsersSet(w http.ResponseWriter, r *http.Request) {
	enc, ok := s.requireValidEncargado(w, r)
	if !ok {
		return
	}
	var body struct {
		UserIDs []int `json:"user_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON payload")
		return
	}
	users, err := s.store.SetAssignedUsers(r.Context(), enc.ID, body.UserIDs)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) handleAdminEncargadoUserAdd(w http.ResponseWriter, r *http.Request) {
	enc, ok := s.requireValidEncargado(w, r)
	if !ok {
		return
	}
	uid, err := strconv.Atoi(r.PathValue("user_id"))
	if err != nil || uid <= 0 {
		writeJSONError(w, http.StatusBadRequest, "Invalid user_id")
		return
	}
	target, err := s.store.GetUserByID(r.Context(), uid)
	if err != nil || target == nil {
		writeJSONError(w, http.StatusNotFound, "User not found")
		return
	}
	if err := s.store.AssignUserToEncargado(r.Context(), enc.ID, uid); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	users, err := s.store.ListAssignedUsers(r.Context(), enc.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) handleAdminEncargadoUserRemove(w http.ResponseWriter, r *http.Request) {
	enc, ok := s.requireValidEncargado(w, r)
	if !ok {
		return
	}
	uid, err := strconv.Atoi(r.PathValue("user_id"))
	if err != nil || uid <= 0 {
		writeJSONError(w, http.StatusBadRequest, "Invalid user_id")
		return
	}
	removed, err := s.store.UnassignUserFromEncargado(r.Context(), enc.ID, uid)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeJSONError(w, http.StatusNotFound, "Assignment not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminTaskList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var allowedUserIDs []int
	if rawUID := strings.TrimSpace(q.Get("user_id")); rawUID != "" {
		parsed, err := strconv.Atoi(rawUID)
		if err != nil || parsed <= 0 {
			writeJSONError(w, http.StatusBadRequest, "Invalid user_id")
			return
		}
		allowedUserIDs = []int{parsed}
	}
	orderType := q.Get("order_type")
	if orderType == "" {
		orderType = "created_at"
	}
	orderBy := q.Get("order_by")
	if orderBy == "" {
		orderBy = "desc"
	}
	results, err := s.store.ListTasks(r.Context(), allowedUserIDs, q.Get("status"), orderType, orderBy)
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
