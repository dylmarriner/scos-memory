package main

import (
	"database/sql"
	"net/http"
	"strings"
)

func (s *server) teams(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT t.id,t.name,m.role,t.tenant_id FROM cloud_teams t JOIN cloud_team_members m ON m.team_id=t.id WHERE m.user_id=$1 ORDER BY t.created_at`, id.UserID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var team, name, role, tenant string
		if rows.Scan(&team, &name, &role, &tenant) == nil {
			out = append(out, map[string]string{"id": team, "name": name, "role": role, "tenant_id": tenant})
		}
	}
	jsonWrite(w, 200, out)
}

func (s *server) createTeam(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if decode(r, &in) != nil || strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 {
		http.Error(w, "team name is required", 400)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback()
	var tenant, team, principal, workspace, namespace string
	if tenant, err = createTenant(r.Context(), tx, strings.TrimSpace(in.Name)); err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO cloud_teams(tenant_id,owner_user_id,name) VALUES($1,$2,$3) RETURNING id`, tenant, id.UserID, strings.TrimSpace(in.Name)).Scan(&team)
	}
	if err == nil {
		err = actAs(r.Context(), tx, tenant, "")
	}
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$3) RETURNING id`, tenant, "team:"+team+":"+id.UserID, id.DisplayName).Scan(&principal)
	}
	if err == nil {
		err = actAs(r.Context(), tx, tenant, principal)
	}
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO workspaces(tenant_id,name) VALUES($1,$2) RETURNING id`, tenant, strings.TrimSpace(in.Name)).Scan(&workspace)
	}
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO namespaces(tenant_id,owner_principal_id,workspace_id,canonical_name) VALUES($1,$2,$3,'team-default') RETURNING id`, tenant, principal, workspace).Scan(&namespace)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO namespace_memberships(namespace_id,principal_id,can_read,can_write_candidates,can_promote,can_administer) VALUES($1,$2,true,true,true,true)`, namespace, principal)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO cloud_team_members(team_id,user_id,principal_id,role) VALUES($1,$2,$3,'owner')`, team, id.UserID, principal)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO cloud_subscriptions(tenant_id,plan,status) VALUES($1,'free','active')`, tenant)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		http.Error(w, "team creation failed", 500)
		return
	}
	jsonWrite(w, 201, map[string]string{"id": team, "tenant_id": tenant, "name": strings.TrimSpace(in.Name), "role": "owner"})
}

func (s *server) teamRole(r *http.Request, teamID string, userID string) (string, error) {
	var role string
	err := s.db.QueryRowContext(r.Context(), `SELECT role FROM cloud_team_members WHERE team_id=$1 AND user_id=$2`, teamID, userID).Scan(&role)
	return role, err
}

func (s *server) inviteTeam(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	var in struct {
		TeamID string `json:"team_id"`
		Email  string `json:"email"`
		Role   string `json:"role"`
	}
	if decode(r, &in) != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	in.TeamID = strings.TrimSpace(in.TeamID)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Role = strings.TrimSpace(in.Role)
	if in.Role == "" {
		in.Role = "member"
	}
	role, e := s.teamRole(r, in.TeamID, id.UserID)
	if e != nil || (role != "owner" && role != "admin") {
		http.Error(w, "team admin access required", 403)
		return
	}
	if in.Email == "" || (in.Role != "member" && in.Role != "admin") {
		http.Error(w, "valid email and role are required", 400)
		return
	}
	var max, members int
	// Lock the team while checking capacity. Without this lock two valid
	// invitations can both observe room and over-allocate the team.
	if e = s.db.QueryRowContext(r.Context(), `SELECT max_members FROM cloud_teams WHERE id=$1 FOR UPDATE`, in.TeamID).Scan(&max); e != nil {
		http.Error(w, "team not found", 404)
		return
	}
	if e = s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM cloud_team_members WHERE team_id=$1`, in.TeamID).Scan(&members); e != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if members >= max {
		http.Error(w, "team member limit reached", 409)
		return
	}
	token, e := randomToken("invite_")
	if e != nil {
		http.Error(w, "invite unavailable", 500)
		return
	}
	var invite string
	e = s.db.QueryRowContext(r.Context(), `INSERT INTO cloud_team_invitations(team_id,email,role,token_hash,invited_by) VALUES($1,$2,$3,$4,$5) RETURNING id`, in.TeamID, in.Email, in.Role, hash(token), id.UserID).Scan(&invite)
	if e != nil {
		http.Error(w, "invite creation failed", 500)
		return
	}
	jsonWrite(w, 201, map[string]string{"id": invite, "email": in.Email, "token": token, "warning": "Deliver this invitation token through your configured email provider; it expires in seven days."})
}

func (s *server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if decode(r, &in) != nil || strings.TrimSpace(in.Token) == "" {
		http.Error(w, "token is required", 400)
		return
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback()
	var invite, team, tenant, role, email string
	e = tx.QueryRowContext(r.Context(), `SELECT i.id,i.team_id,t.tenant_id,i.role,i.email FROM cloud_team_invitations i JOIN cloud_teams t ON t.id=i.team_id WHERE i.token_hash=$1 AND i.accepted_at IS NULL AND i.expires_at>now() FOR UPDATE OF i`, hash(in.Token)).Scan(&invite, &team, &tenant, &role, &email)
	if e != nil || !strings.EqualFold(email, id.Email) {
		http.Error(w, "invitation is invalid or not for this account", 403)
		return
	}
	var max, members int
	e = tx.QueryRowContext(r.Context(), `SELECT max_members FROM cloud_teams WHERE id=$1 FOR UPDATE`, team).Scan(&max)
	if e == nil {
		e = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM cloud_team_members WHERE team_id=$1`, team).Scan(&members)
	}
	if e != nil || members >= max {
		http.Error(w, "team member limit reached", 409)
		return
	}
	var principal string
	e = actAs(r.Context(), tx, tenant, "")
	if e != nil {
		http.Error(w, "invitation acceptance failed", 500)
		return
	}
	e = tx.QueryRowContext(r.Context(), `INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$3) ON CONFLICT (tenant_id,external_id) DO UPDATE SET display_name=EXCLUDED.display_name RETURNING id`, tenant, "team:"+team+":"+id.UserID, id.DisplayName).Scan(&principal)
	if e == nil {
		_, e = tx.ExecContext(r.Context(), `INSERT INTO cloud_team_members(team_id,user_id,principal_id,role) VALUES($1,$2,$3,$4) ON CONFLICT (team_id,user_id) DO UPDATE SET role=EXCLUDED.role`, team, id.UserID, principal, role)
	}
	if e == nil {
		_, e = tx.ExecContext(r.Context(), `UPDATE cloud_team_invitations SET accepted_at=now() WHERE id=$1`, invite)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		http.Error(w, "invitation acceptance failed", 500)
		return
	}
	jsonWrite(w, 200, map[string]string{"team_id": team, "role": role, "tenant_id": tenant})
}

func (s *server) teamMembers(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	teamID := strings.TrimSpace(r.URL.Query().Get("team_id"))
	if teamID == "" {
		http.Error(w, "team_id is required", 400)
		return
	}
	if _, err = s.teamRole(r, teamID, id.UserID); err != nil {
		http.Error(w, "team membership required", 403)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT m.user_id,u.email,u.display_name,m.role,m.joined_at FROM cloud_team_members m JOIN cloud_users u ON u.id=m.user_id WHERE m.team_id=$1 ORDER BY m.joined_at`, teamID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var uid, email, name, role string
		var joined sql.NullTime
		if rows.Scan(&uid, &email, &name, &role, &joined) == nil {
			out = append(out, map[string]any{"user_id": uid, "email": email, "display_name": name, "role": role, "joined_at": joined.Time})
		}
	}
	jsonWrite(w, 200, out)
}
