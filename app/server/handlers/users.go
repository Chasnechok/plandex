package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"plandex-server/db"
	"strings"

	shared "plandex-shared"

	"github.com/gorilla/mux"
)

func ListUsersHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received a request for ListUsersHandler")

	if os.Getenv("GOENV") == "development" && os.Getenv("LOCAL_MODE") == "1" {
		writeApiError(w, shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusForbidden,
			Msg:    "Local mode is not supported for user management",
		})
		return
	}

	auth := Authenticate(w, r, true)
	if auth == nil {
		return
	}

	org, err := db.GetOrg(auth.OrgId)
	if err != nil {
		log.Printf("Error getting org: %v\n", err)
		http.Error(w, "Error getting org: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if org.IsTrial {
		writeApiError(w, shared.ApiError{
			Type:   shared.ApiErrorTypeTrialActionNotAllowed,
			Status: http.StatusForbidden,
			Msg:    "Trial user can't list users",
		})
		return
	}

	users, err := db.ListUsers(auth.OrgId)
	if err != nil {
		log.Println("Error listing users: ", err)
		http.Error(w, "Error listing users: "+err.Error(), http.StatusInternalServerError)
		return
	}

	apiUsers := make([]*shared.User, 0, len(users))
	for _, user := range users {
		apiUsers = append(apiUsers, user.ToApi())
	}

	orgUsers, err := db.ListOrgUsers(auth.OrgId)
	if err != nil {
		log.Println("Error listing org users: ", err)
		http.Error(w, "Error listing org users: "+err.Error(), http.StatusInternalServerError)
		return
	}

	orgUsersByUserId := make(map[string]*shared.OrgUser)
	for _, orgUser := range orgUsers {
		orgUsersByUserId[orgUser.UserId] = orgUser.ToApi()
	}

	resp := shared.ListUsersResponse{
		Users:            apiUsers,
		OrgUsersByUserId: orgUsersByUserId,
	}

	bytes, err := json.Marshal(resp)

	if err != nil {
		log.Println("Error marshalling users: ", err)
		http.Error(w, "Error marshalling users: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Println("Successfully processed request for ListUsersHandler")

	w.Write(bytes)
}

func DeleteOrgUserHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received a request for DeleteOrgUserHandler")

	if os.Getenv("GOENV") == "development" && os.Getenv("LOCAL_MODE") == "1" {
		writeApiError(w, shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusForbidden,
			Msg:    "Local mode is not supported for user management",
		})
		return
	}

	auth := Authenticate(w, r, true)
	if auth == nil {
		return
	}

	org, err := db.GetOrg(auth.OrgId)
	if err != nil {
		log.Printf("Error getting org: %v\n", err)
		http.Error(w, "Error getting org: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if org.IsTrial {
		writeApiError(w, shared.ApiError{
			Type:   shared.ApiErrorTypeTrialActionNotAllowed,
			Status: http.StatusForbidden,
			Msg:    "Trial user can't delete users",
		})
		return
	}

	vars := mux.Vars(r)
	userId := vars["userId"]

	log.Println("userId: ", userId)

	orgUser, err := db.GetOrgUser(userId, auth.OrgId)

	if err != nil {
		log.Printf("Error getting org user: %v\n", err)
		http.Error(w, "Error getting org user: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// ensure current user can remove target user
	removePermission := shared.Permission(strings.Join([]string{string(shared.PermissionRemoveUser), orgUser.OrgRoleId}, "|"))

	if !auth.HasPermission(removePermission) {
		log.Printf("User does not have permission to remove user with role: %v\n", orgUser.OrgRoleId)
		http.Error(w, "User does not have permission to remove user with role: "+orgUser.OrgRoleId, http.StatusForbidden)
		return
	}

	// verify user is org member
	isMember, err := db.ValidateOrgMembership(userId, auth.OrgId)

	if err != nil {
		log.Printf("Error validating org membership: %v\n", err)
		http.Error(w, "Error validating org membership: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if !isMember {
		log.Printf("User %s is not a member of org %s\n", userId, auth.OrgId)
		http.Error(w, "User "+userId+" is not a member of org "+auth.OrgId, http.StatusForbidden)
		return
	}

	orgOwnerRoleId, err := db.GetOrgOwnerRoleId()

	if err != nil {
		log.Printf("Error getting org owner role id: %v\n", err)
		http.Error(w, "Error getting org owner role id: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// verify user isn't the only org owner
	if orgUser.OrgRoleId == orgOwnerRoleId {
		numOwners, err := db.NumUsersWithRole(auth.OrgId, orgOwnerRoleId)

		if err != nil {
			log.Printf("Error getting number of org owners: %v\n", err)
			http.Error(w, "Error getting number of org owners: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if numOwners == 1 {
			log.Println("Cannot delete the only org owner")
			http.Error(w, "Cannot delete the only org owner", http.StatusForbidden)
			return
		}
	}

	// start a transaction
	tx, err := db.Conn.Beginx()
	if err != nil {
		log.Printf("Error starting transaction: %v\n", err)
		http.Error(w, "Error starting transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Ensure that rollback is attempted in case of failure
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil {
			if rbErr == sql.ErrTxDone {
				log.Println("attempted to roll back transaction, but it was already committed")
			} else {
				log.Printf("transaction rollback error: %v\n", rbErr)
			}
		} else {
			log.Println("transaction rolled back")
		}
	}()

	err = db.DeleteOrgUser(auth.OrgId, userId, tx)

	if err != nil {
		log.Println("Error deleting org user: ", err)
		http.Error(w, "Error deleting org user: "+err.Error(), http.StatusInternalServerError)
		return
	}

	invite, err := db.GetActiveInviteByEmail(auth.OrgId, auth.User.Email)

	if err != nil {
		log.Println("Error getting invite for org user: ", err)
		http.Error(w, "Error getting invite for org user: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if invite != nil {
		err = db.DeleteInvite(invite.Id, tx)

		if err != nil {
			log.Println("Error deleting invite: ", err)
			http.Error(w, "Error deleting invite: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()

	if err != nil {
		log.Println("Error committing transaction: ", err)
		http.Error(w, "Error committing transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Println("Successfully processed request for DeleteOrgUserHandler")
}
