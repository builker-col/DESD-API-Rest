package httpapi

import (
	"bytes"
	"net/http"
	"reflect"
	"testing"
	"time"

	"desd-api/internal/db"
	"desd-api/internal/export"
	"github.com/xuri/excelize/v2"
)

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }

func TestUserEffectiveRole(t *testing.T) {
	tests := []struct {
		name      string
		user      *db.User
		wantRole  string
		wantAdmin bool
		wantEnc   bool
	}{
		{"nil user", nil, db.RoleUser, false, false},
		{"explicit admin", &db.User{Role: db.RoleAdmin}, db.RoleAdmin, true, false},
		{"legacy is_admin flag", &db.User{Role: db.RoleUser, IsAdmin: true}, db.RoleAdmin, true, false},
		{"encargado", &db.User{Role: db.RoleEncargado}, db.RoleEncargado, false, true},
		{"regular user", &db.User{Role: db.RoleUser}, db.RoleUser, false, false},
		{"empty role defaults to user", &db.User{}, db.RoleUser, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.user.EffectiveRole(); got != tc.wantRole {
				t.Errorf("EffectiveRole() = %q, want %q", got, tc.wantRole)
			}
			if got := tc.user.IsRoleAdmin(); got != tc.wantAdmin {
				t.Errorf("IsRoleAdmin() = %v, want %v", got, tc.wantAdmin)
			}
			if got := tc.user.IsRoleEncargado(); got != tc.wantEnc {
				t.Errorf("IsRoleEncargado() = %v, want %v", got, tc.wantEnc)
			}
		})
	}
}

func TestResolveTaskScope(t *testing.T) {
	admin := &db.User{ID: 1, Role: db.RoleAdmin, IsAdmin: true}
	encargado := &db.User{ID: 2, Role: db.RoleEncargado}
	regular := &db.User{ID: 3, Role: db.RoleUser}

	t.Run("admin sees all tasks by default", func(t *testing.T) {
		ids, status, _ := resolveTaskScope(admin, nil, nil)
		if status != 0 || ids != nil {
			t.Fatalf("got ids=%v status=%d, want nil and 0", ids, status)
		}
	})

	t.Run("admin can filter by any user_id", func(t *testing.T) {
		ids, status, _ := resolveTaskScope(admin, intPtr(99), nil)
		if status != 0 || !reflect.DeepEqual(ids, []int{99}) {
			t.Fatalf("got ids=%v status=%d, want [99] and 0", ids, status)
		}
	})

	t.Run("encargado sees own and assigned users tasks", func(t *testing.T) {
		ids, status, _ := resolveTaskScope(encargado, nil, []int{3, 4})
		if status != 0 || !reflect.DeepEqual(ids, []int{2, 3, 4}) {
			t.Fatalf("got ids=%v status=%d, want [2 3 4] and 0", ids, status)
		}
	})

	t.Run("encargado can filter by assigned user", func(t *testing.T) {
		ids, status, _ := resolveTaskScope(encargado, intPtr(4), []int{3, 4})
		if status != 0 || !reflect.DeepEqual(ids, []int{4}) {
			t.Fatalf("got ids=%v status=%d, want [4] and 0", ids, status)
		}
	})

	t.Run("encargado can filter by own id", func(t *testing.T) {
		ids, status, _ := resolveTaskScope(encargado, intPtr(2), []int{3, 4})
		if status != 0 || !reflect.DeepEqual(ids, []int{2}) {
			t.Fatalf("got ids=%v status=%d, want [2] and 0", ids, status)
		}
	})

	t.Run("encargado forbidden from filtering unassigned user", func(t *testing.T) {
		_, status, _ := resolveTaskScope(encargado, intPtr(99), []int{3, 4})
		if status != http.StatusForbidden {
			t.Fatalf("got status=%d, want %d", status, http.StatusForbidden)
		}
	})

	t.Run("regular user sees only own tasks", func(t *testing.T) {
		ids, status, _ := resolveTaskScope(regular, nil, nil)
		if status != 0 || !reflect.DeepEqual(ids, []int{3}) {
			t.Fatalf("got ids=%v status=%d, want [3] and 0", ids, status)
		}
	})

	t.Run("regular user forbidden from filtering other user", func(t *testing.T) {
		_, status, _ := resolveTaskScope(regular, intPtr(2), nil)
		if status != http.StatusForbidden {
			t.Fatalf("got status=%d, want %d", status, http.StatusForbidden)
		}
	})
}

func TestResolveRoleForCreate(t *testing.T) {
	role, err := resolveRoleForCreate(strPtr("ENCARGADO"), false)
	if err != nil || role != db.RoleEncargado {
		t.Fatalf("got (%q, %v), want (%q, nil)", role, err, db.RoleEncargado)
	}

	role, err = resolveRoleForCreate(nil, true)
	if err != nil || role != db.RoleAdmin {
		t.Fatalf("got (%q, %v), want (%q, nil)", role, err, db.RoleAdmin)
	}

	role, err = resolveRoleForCreate(nil, false)
	if err != nil || role != db.RoleUser {
		t.Fatalf("got (%q, %v), want (%q, nil)", role, err, db.RoleUser)
	}

	if _, err := resolveRoleForCreate(strPtr("superadmin"), false); err == nil {
		t.Fatal("expected error for invalid role")
	}
}

func TestResolveRoleForUpdate(t *testing.T) {
	enc := &db.User{ID: 2, Role: db.RoleEncargado, IsAdmin: false}
	adm := &db.User{ID: 1, Role: db.RoleAdmin, IsAdmin: true}

	// Updating is_admin=false on an encargado should not clobber encargado role
	role, err := resolveRoleForUpdate(enc, nil, boolPtr(false))
	if err != nil || role != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", role, err)
	}

	// Demoting an admin with legacy is_admin=false sets role to user
	role, err = resolveRoleForUpdate(adm, nil, boolPtr(false))
	if err != nil || role == nil || *role != db.RoleUser {
		t.Fatalf("got (%v, %v), want (%q, nil)", role, err, db.RoleUser)
	}

	// Promoting to encargado explicitly
	role, err = resolveRoleForUpdate(adm, strPtr("encargado"), nil)
	if err != nil || role == nil || *role != db.RoleEncargado {
		t.Fatalf("got (%v, %v), want (%q, nil)", role, err, db.RoleEncargado)
	}

	// Invalid role
	if _, err := resolveRoleForUpdate(enc, strPtr("invalid"), nil); err == nil {
		t.Fatal("expected error for invalid role")
	}
}

func TestResolveReportUserScope(t *testing.T) {
	admin := &db.User{ID: 1, Role: db.RoleAdmin, IsAdmin: true}
	encargado := &db.User{ID: 2, Role: db.RoleEncargado}
	regular := &db.User{ID: 3, Role: db.RoleUser}

	ids, status, _ := resolveReportUserScope(admin, nil, nil)
	if status != 0 || ids != nil {
		t.Fatalf("admin all: got ids=%v status=%d", ids, status)
	}

	ids, status, _ = resolveReportUserScope(admin, []int{3, 4}, nil)
	if status != 0 || !reflect.DeepEqual(ids, []int{3, 4}) {
		t.Fatalf("admin selected: got ids=%v status=%d", ids, status)
	}

	ids, status, _ = resolveReportUserScope(encargado, nil, []int{3, 4})
	if status != 0 || !reflect.DeepEqual(ids, []int{2, 3, 4}) {
		t.Fatalf("encargado all: got ids=%v status=%d", ids, status)
	}

	ids, status, _ = resolveReportUserScope(encargado, []int{3}, []int{3, 4})
	if status != 0 || !reflect.DeepEqual(ids, []int{3}) {
		t.Fatalf("encargado selected valid: got ids=%v status=%d", ids, status)
	}

	_, status, _ = resolveReportUserScope(encargado, []int{3, 99}, []int{3, 4})
	if status != http.StatusForbidden {
		t.Fatalf("encargado selected invalid: got status=%d, want 403", status)
	}

	_, status, _ = resolveReportUserScope(regular, nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("regular user: got status=%d, want 403", status)
	}
}

func TestWriteFilesExcelReport(t *testing.T) {
	now := time.Date(2026, 11, 27, 23, 30, 0, 0, time.UTC)
	rows := []db.FileReportRow{
		{
			UserID: 2, Username: "juan", UserRole: "user", TaskID: "lote_1",
			Filename: "doc1.pdf", FilePath: "/hotfolder/done/lote_1/doc1.pdf",
			Source: "hotfolder", Models: "cude,rode", Status: "COMPLETED",
			Pages: 5, SizeBytes: 20480, AlertsCount: 2, ProcessedAt: now,
		},
	}

	var buf bytes.Buffer
	if err := export.WriteFilesExcelReport(&buf, rows); err != nil {
		t.Fatalf("WriteFilesExcelReport failed: %v", err)
	}

	wb, err := excelize.OpenReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer wb.Close()

	sheets := wb.GetSheetList()
	if !reflect.DeepEqual(sheets, []string{"Archivos Procesados", "Resumen por Usuario"}) {
		t.Fatalf("unexpected sheets: %v", sheets)
	}
	val, err := wb.GetCellValue("Archivos Procesados", "D2")
	if err != nil || val != "doc1.pdf" {
		t.Fatalf("expected D2=doc1.pdf, got %q (err=%v)", val, err)
	}
	sumVal, err := wb.GetCellValue("Resumen por Usuario", "D2")
	if err != nil || sumVal != "1" {
		t.Fatalf("expected Resumen D2=1, got %q (err=%v)", sumVal, err)
	}
}
