package feed

import (
	"time"

	"github.com/skobkin/magnetico-web/internal/web/password"
)

func newPasswordService() *password.Service {
	return password.NewService(password.NewLimiter(1, time.Minute), password.Params{Memory: 64, Time: 1, Threads: 1})
}
