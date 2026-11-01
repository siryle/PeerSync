//go:build !unix

package syncer

import "os"

const readFileFlags = os.O_RDONLY
