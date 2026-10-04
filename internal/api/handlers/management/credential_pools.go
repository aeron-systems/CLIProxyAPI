package management

import (
	"net/http"

	"github.com/gin-gonic/gin"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetCredentialPools reports the pool of every client key and credential so a
// dashboard can show which keys may use which subscription accounts.
func (h *Handler) GetCredentialPools(c *gin.Context) {
	pools := coreauth.CurrentCredentialPools()
	if pools == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "pools": []gin.H{}})
		return
	}
	resp := gin.H{"enabled": true, "default-pool": pools.DefaultPool()}
	if errInvalid := pools.Invalid(); errInvalid != nil {
		resp["error"] = errInvalid.Error()
		resp["pools"] = []gin.H{}
		c.JSON(http.StatusOK, resp)
		return
	}
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.List()
	}
	clients := make([]gin.H, 0)
	for _, client := range pools.Clients() {
		clients = append(clients, gin.H{"name": client.Name, "pool": client.Pool, "api-key": maskPoolKey(client.APIKey)})
	}
	resp["clients"] = clients
	// Each pool lists its masked client keys and the signed-in credentials it may
	// use (auth file names and account emails), the shape the dashboard reads.
	poolList := make([]gin.H, 0)
	for _, def := range pools.Definitions() {
		keys, names := make([]string, 0), make([]string, 0)
		for _, client := range pools.Clients() {
			if client.Pool == def.Name {
				keys = append(keys, maskPoolKey(client.APIKey))
				names = append(names, client.Name)
			}
		}
		accounts := make([]string, 0)
		for _, auth := range auths {
			if auth == nil || !pools.Allows(def.Name, auth) {
				continue
			}
			accounts = append(accounts, auth.ID)
			if _, account := auth.AccountInfo(); account != "" && account != auth.ID {
				accounts = append(accounts, account)
			}
		}
		poolList = append(poolList, gin.H{
			"name":        def.Name,
			"reserved":    def.Reserved,
			"fallback":    def.Fallback,
			"credentials": def.Credentials,
			"accounts":    accounts,
			"api-keys":    keys,
			"clients":     names,
		})
	}
	resp["pools"] = poolList
	credentials := make([]gin.H, 0)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		_, account := auth.AccountInfo()
		credentials = append(credentials, gin.H{
			"id":          auth.ID,
			"provider":    auth.Provider,
			"account":     account,
			"pools":       pools.PoolsFor(auth),
			"reserved-by": pools.ReservedBy(auth),
		})
	}
	resp["credentials"] = credentials
	c.JSON(http.StatusOK, resp)
}

func maskPoolKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}
