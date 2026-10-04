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
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}
	resp := gin.H{"enabled": true, "default-pool": pools.DefaultPool()}
	if errInvalid := pools.Invalid(); errInvalid != nil {
		resp["error"] = errInvalid.Error()
		c.JSON(http.StatusOK, resp)
		return
	}
	clients := make([]gin.H, 0)
	for _, client := range pools.Clients() {
		clients = append(clients, gin.H{"name": client.Name, "pool": client.Pool, "api-key": maskPoolKey(client.APIKey)})
	}
	resp["clients"] = clients
	resp["pools"] = pools.Definitions()
	credentials := make([]gin.H, 0)
	if h != nil && h.authManager != nil {
		for _, auth := range h.authManager.List() {
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
