package main

// REST handlers for Keep are implemented here.
//
// GetBlockHandler (GET /locator)
// PutBlockHandler (PUT /locator)
// IndexHandler    (GET /index, GET /index/prefix)
// StatusHandler   (GET /status.json)

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MakeRESTRouter returns a new mux.Router that forwards all Keep
// requests to the appropriate handlers.
//
func MakeRESTRouter() *mux.Router {
	rest := mux.NewRouter()

	rest.HandleFunc(
		`/{hash:[0-9a-f]{32}}`, GetBlockHandler).Methods("GET", "HEAD")
	rest.HandleFunc(
		`/{hash:[0-9a-f]{32}}+{hints}`,
		GetBlockHandler).Methods("GET", "HEAD")

	rest.HandleFunc(`/{hash:[0-9a-f]{32}}`, PutBlockHandler).Methods("PUT")
	rest.HandleFunc(`/{hash:[0-9a-f]{32}}`, DeleteHandler).Methods("DELETE")

	// For IndexHandler we support:
	//   /index           - returns all locators
	//   /index/{prefix}  - returns all locators that begin with {prefix}
	//      {prefix} is a string of hexadecimal digits between 0 and 32 digits.
	//      If {prefix} is the empty string, return an index of all locators
	//      (so /index and /index/ behave identically)
	//      A client may supply a full 32-digit locator string, in which
	//      case the server will return an index with either zero or one
	//      entries. This usage allows a client to check whether a block is
	//      present, and its size and upload time, without retrieving the
	//      entire block.
	//
	rest.HandleFunc(`/index`, IndexHandler).Methods("GET", "HEAD")
	rest.HandleFunc(
		`/index/{prefix:[0-9a-f]{0,32}}`, IndexHandler).Methods("GET", "HEAD")
	rest.HandleFunc(`/status.json`, StatusHandler).Methods("GET", "HEAD")

	// Any request which does not match any of these routes gets
	// 400 Bad Request.
	rest.NotFoundHandler = http.HandlerFunc(BadRequestHandler)

	return rest
}

func BadRequestHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, BadRequestError.Error(), BadRequestError.HTTPCode)
}

// FindKeepVolumes scans all mounted volumes on the system for Keep
// volumes, and returns a list of matching paths.
//
// A device is assumed to be a Keep volume if it is a normal or tmpfs
// volume and has a "/keep" directory directly underneath the mount
// point.
//
func FindKeepVolumes() []string {
	vols := make([]string, 0)

	if f, err := os.Open(PROC_MOUNTS); err != nil {
		log.Fatalf("opening %s: %s\n", PROC_MOUNTS, err)
	} else {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			args := strings.Fields(scanner.Text())
			dev, mount := args[0], args[1]
			if mount != "/" &&
				(dev == "tmpfs" || strings.HasPrefix(dev, "/dev/")) {
				keep := mount + "/keep"
				if st, err := os.Stat(keep); err == nil && st.IsDir() {
					vols = append(vols, keep)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			log.Fatal(err)
		}
	}
	return vols
}

func GetBlockHandler(resp http.ResponseWriter, req *http.Request) {
	hash := mux.Vars(req)["hash"]

	log.Printf("%s %s", req.Method, hash)

	hints := mux.Vars(req)["hints"]

	// Parse the locator string and hints from the request.
	// TODO(twp): implement a Locator type.
	var signature, timestamp string
	if hints != "" {
		signature_pat, _ := regexp.Compile("^A([[:xdigit:]]+)@([[:xdigit:]]{8})$")
		for _, hint := range strings.Split(hints, "+") {
			if match, _ := regexp.MatchString("^[[:digit:]]+$", hint); match {
				// Server ignores size hints
			} else if m := signature_pat.FindStringSubmatch(hint); m != nil {
				signature = m[1]
				timestamp = m[2]
			} else if match, _ := regexp.MatchString("^[[:upper:]]", hint); match {
				// Any unknown hint that starts with an uppercase letter is
				// presumed to be valid and ignored, to permit forward compatibility.
			} else {
				// Unknown format; not a valid locator.
				http.Error(resp, BadRequestError.Error(), BadRequestError.HTTPCode)
				return
			}
		}
	}

	// If permission checking is in effect, verify this
	// request's permission signature.
	if enforce_permissions {
		if signature == "" || timestamp == "" {
			http.Error(resp, PermissionError.Error(), PermissionError.HTTPCode)
			return
		} else if IsExpired(timestamp) {
			http.Error(resp, ExpiredError.Error(), ExpiredError.HTTPCode)
			return
		} else {
			req_locator := req.URL.Path[1:] // strip leading slash
			if !VerifySignature(req_locator, GetApiToken(req)) {
				http.Error(resp, PermissionError.Error(), PermissionError.HTTPCode)
				return
			}
		}
	}

	block, err := GetBlock(hash)

	// Garbage collect after each GET. Fixes #2865.
	// TODO(twp): review Keep memory usage and see if there's
	// a better way to do this than blindly garbage collecting
	// after every block.
	defer runtime.GC()

	if err != nil {
		// This type assertion is safe because the only errors
		// GetBlock can return are DiskHashError or NotFoundError.
		if err == NotFoundError {
			log.Printf("%s: not found, giving up\n", hash)
		}
		http.Error(resp, err.Error(), err.(*KeepError).HTTPCode)
		return
	}

	resp.Header().Set("X-Block-Size", fmt.Sprintf("%d", len(block)))

	_, err = resp.Write(block)
	if err != nil {
		log.Printf("GetBlockHandler: writing response: %s", err)
	}

	return
}

func PutBlockHandler(resp http.ResponseWriter, req *http.Request) {
	// Garbage collect after each PUT. Fixes #2865.
	// See also GetBlockHandler.
	defer runtime.GC()

	hash := mux.Vars(req)["hash"]

	log.Printf("%s %s", req.Method, hash)

	// Read the block data to be stored.
	// If the request exceeds BLOCKSIZE bytes, issue a HTTP 500 error.
	//
	if req.ContentLength > BLOCKSIZE {
		http.Error(resp, TooLongError.Error(), TooLongError.HTTPCode)
		return
	}

	buf := make([]byte, req.ContentLength)
	nread, err := io.ReadFull(req.Body, buf)
	if err != nil {
		http.Error(resp, err.Error(), 500)
	} else if int64(nread) < req.ContentLength {
		http.Error(resp, "request truncated", 500)
	} else {
		if err := PutBlock(buf, hash); err == nil {
			// Success; add a size hint, sign the locator if
			// possible, and return it to the client.
			return_hash := fmt.Sprintf("%s+%d", hash, len(buf))
			api_token := GetApiToken(req)
			if PermissionSecret != nil && api_token != "" {
				expiry := time.Now().Add(permission_ttl)
				return_hash = SignLocator(return_hash, api_token, expiry)
			}
			resp.Write([]byte(return_hash + "\n"))
		} else {
			ke := err.(*KeepError)
			http.Error(resp, ke.Error(), ke.HTTPCode)
		}
	}
	return
}

// IndexHandler
//     A HandleFunc to address /index and /index/{prefix} requests.
//
func IndexHandler(resp http.ResponseWriter, req *http.Request) {
	prefix := mux.Vars(req)["prefix"]

	// Only the data manager may issue /index requests,
	// and only if enforce_permissions is enabled.
	// All other requests return 403 Forbidden.
	api_token := GetApiToken(req)
	if !enforce_permissions ||
		api_token == "" ||
		data_manager_token != api_token {
		http.Error(resp, PermissionError.Error(), PermissionError.HTTPCode)
		return
	}
	var index string
	for _, vol := range KeepVM.Volumes() {
		index = index + vol.Index(prefix)
	}
	resp.Write([]byte(index))
}

// StatusHandler
//     Responds to /status.json requests with the current node status,
//     described in a JSON structure.
//
//     The data given in a status.json response includes:
//        volumes - a list of Keep volumes currently in use by this server
//          each volume is an object with the following fields:
//            * mount_point
//            * device_num (an integer identifying the underlying filesystem)
//            * bytes_free
//            * bytes_used
//
type VolumeStatus struct {
	MountPoint string `json:"mount_point"`
	DeviceNum  uint64 `json:"device_num"`
	BytesFree  uint64 `json:"bytes_free"`
	BytesUsed  uint64 `json:"bytes_used"`
}

type NodeStatus struct {
	Volumes []*VolumeStatus `json:"volumes"`
}

func StatusHandler(resp http.ResponseWriter, req *http.Request) {
	st := GetNodeStatus()
	if jstat, err := json.Marshal(st); err == nil {
		resp.Write(jstat)
	} else {
		log.Printf("json.Marshal: %s\n", err)
		log.Printf("NodeStatus = %v\n", st)
		http.Error(resp, err.Error(), 500)
	}
}

// GetNodeStatus
//     Returns a NodeStatus struct describing this Keep
//     node's current status.
//
func GetNodeStatus() *NodeStatus {
	st := new(NodeStatus)

	st.Volumes = make([]*VolumeStatus, len(KeepVM.Volumes()))
	for i, vol := range KeepVM.Volumes() {
		st.Volumes[i] = vol.Status()
	}
	return st
}

// GetVolumeStatus
//     Returns a VolumeStatus describing the requested volume.
//
func GetVolumeStatus(volume string) *VolumeStatus {
	var fs syscall.Statfs_t
	var devnum uint64

	if fi, err := os.Stat(volume); err == nil {
		devnum = fi.Sys().(*syscall.Stat_t).Dev
	} else {
		log.Printf("GetVolumeStatus: os.Stat: %s\n", err)
		return nil
	}

	err := syscall.Statfs(volume, &fs)
	if err != nil {
		log.Printf("GetVolumeStatus: statfs: %s\n", err)
		return nil
	}
	// These calculations match the way df calculates disk usage:
	// "free" space is measured by fs.Bavail, but "used" space
	// uses fs.Blocks - fs.Bfree.
	free := fs.Bavail * uint64(fs.Bsize)
	used := (fs.Blocks - fs.Bfree) * uint64(fs.Bsize)
	return &VolumeStatus{volume, devnum, free, used}
}

// DeleteHandler processes DELETE requests.
//
// DELETE /{hash:[0-9a-f]{32} will delete the block with the specified hash
// from all connected volumes.
//
// Only the Data Manager, or an Arvados admin with scope "all", are
// allowed to issue DELETE requests.  If a DELETE request is not
// authenticated or is issued by a non-admin user, the server returns
// a PermissionError.
//
// Upon receiving a valid request from an authorized user,
// DeleteHandler deletes all copies of the specified block on local
// writable volumes.
//
// Response format:
//
// If the requested blocks was not found on any volume, the response
// code is HTTP 404 Not Found.
//
// Otherwise, the response code is 200 OK, with a response body
// consisting of the JSON message
//
//    {"copies_deleted":d,"copies_failed":f}
//
// where d and f are integers representing the number of blocks that
// were successfully and unsuccessfully deleted.
//
func DeleteHandler(resp http.ResponseWriter, req *http.Request) {
	hash := mux.Vars(req)["hash"]
	log.Printf("%s %s", req.Method, hash)

	// Confirm that this user is an admin and has a token with unlimited scope.
	var tok = GetApiToken(req)
	if tok == "" || !CanDelete(tok) {
		http.Error(resp, PermissionError.Error(), PermissionError.HTTPCode)
		return
	}

	if never_delete {
		http.Error(resp, MethodDisabledError.Error(), MethodDisabledError.HTTPCode)
		return
	}

	// Delete copies of this block from all available volumes.  Report
	// how many blocks were successfully and unsuccessfully
	// deleted.
	var result struct {
		Deleted int `json:"copies_deleted"`
		Failed  int `json:"copies_failed"`
	}
	for _, vol := range KeepVM.Volumes() {
		if err := vol.Delete(hash); err == nil {
			result.Deleted++
		} else if os.IsNotExist(err) {
			continue
		} else {
			result.Failed++
			log.Println("DeleteHandler:", err)
		}
	}

	var st int

	if result.Deleted == 0 && result.Failed == 0 {
		st = http.StatusNotFound
	} else {
		st = http.StatusOK
	}

	resp.WriteHeader(st)

	if st == http.StatusOK {
		if body, err := json.Marshal(result); err == nil {
			resp.Write(body)
		} else {
			log.Printf("json.Marshal: %s (result = %v)\n", err, result)
			http.Error(resp, err.Error(), 500)
		}
	}
}

// GetBlock, PutBlock and TouchBlock implement lower-level code for
// handling blocks by rooting through volumes connected to the local
// machine.  Once the handler has determined that system policy permits
// the request, it calls these methods to perform the actual operation.
//
// TODO(twp): this code would probably be better located in the
// VolumeManager interface. As an abstraction, the VolumeManager
// should be the only part of the code that cares about which volume a
// block is stored on, so it should be responsible for figuring out
// which volume to check for fetching blocks, storing blocks, etc.

func GetBlock(hash string) ([]byte, error) {
	// Attempt to read the requested hash from a keep volume.
	error_to_caller := NotFoundError

	for _, vol := range KeepVM.Volumes() {
		if buf, err := vol.Get(hash); err != nil {
			// IsNotExist is an expected error and may be ignored.
			// (If all volumes report IsNotExist, we return a NotFoundError)
			// All other errors should be logged but we continue trying to
			// read.
			switch {
			case os.IsNotExist(err):
				continue
			default:
				log.Printf("GetBlock: reading %s: %s\n", hash, err)
			}
		} else {
			// Double check the file checksum.
			//
			filehash := fmt.Sprintf("%x", md5.Sum(buf))
			if filehash != hash {
				// TODO(twp): this condition probably represents a bad disk and
				// should raise major alarm bells for an administrator: e.g.
				// they should be sent directly to an event manager at high
				// priority or logged as urgent problems.
				//
				log.Printf("%s: checksum mismatch for request %s (actual %s)\n",
					vol, hash, filehash)
				error_to_caller = DiskHashError
			} else {
				// Success!
				if error_to_caller != NotFoundError {
					log.Printf("%s: checksum mismatch for request %s but a good copy was found on another volume and returned\n",
						vol, hash)
				}
				return buf, nil
			}
		}
	}

	if error_to_caller != NotFoundError {
		log.Printf("%s: checksum mismatch, no good copy found\n", hash)
	}
	return nil, error_to_caller
}

/* PutBlock(block, hash)
   Stores the BLOCK (identified by the content id HASH) in Keep.

   The MD5 checksum of the block must be identical to the content id HASH.
   If not, an error is returned.

   PutBlock stores the BLOCK on the first Keep volume with free space.
   A failure code is returned to the user only if all volumes fail.

   On success, PutBlock returns nil.
   On failure, it returns a KeepError with one of the following codes:

   500 Collision
          A different block with the same hash already exists on this
          Keep server.
   422 MD5Fail
          The MD5 hash of the BLOCK does not match the argument HASH.
   503 Full
          There was not enough space left in any Keep volume to store
          the object.
   500 Fail
          The object could not be stored for some other reason (e.g.
          all writes failed). The text of the error message should
          provide as much detail as possible.
*/

func PutBlock(block []byte, hash string) error {
	// Check that BLOCK's checksum matches HASH.
	blockhash := fmt.Sprintf("%x", md5.Sum(block))
	if blockhash != hash {
		log.Printf("%s: MD5 checksum %s did not match request", hash, blockhash)
		return RequestHashError
	}

	// If we already have a block on disk under this identifier, return
	// success (but check for MD5 collisions).
	// The only errors that GetBlock can return are DiskHashError and NotFoundError.
	// In either case, we want to write our new (good) block to disk,
	// so there is nothing special to do if err != nil.
	if oldblock, err := GetBlock(hash); err == nil {
		if bytes.Compare(block, oldblock) == 0 {
			// The block already exists; update the timestamp and
			// return.
			// Note that TouchBlock will fail (and therefore
			// so will PutBlock) if the block exists but is found on a
			// read-only volume. That is intentional: if the user has
			// requested N replicas of a block, we want to be sure that
			// there are at least N *writable* replicas, so a block
			// that cannot be written to should not count toward the
			// replication total.
			return TouchBlock(block)
		} else {
			return CollisionError
		}
	}

	// Choose a Keep volume to write to.
	// If this volume fails, try all of the volumes in order.
	vol := KeepVM.Choose()
	if err := vol.Put(hash, block); err == nil {
		return nil // success!
	} else {
		allFull := true
		for _, vol := range KeepVM.Volumes() {
			err := vol.Put(hash, block)
			if err == nil {
				return nil // success!
			}
			if err != FullError {
				// The volume is not full but the write did not succeed.
				// Report the error and continue trying.
				allFull = false
				log.Printf("%s: Write(%s): %s\n", vol, hash, err)
			}
		}

		if allFull {
			log.Printf("all Keep volumes full")
			return FullError
		} else {
			log.Printf("all Keep volumes failed")
			return GenericError
		}
	}
}

// TouchBlock finds the block identified by hash and updates its
// filesystem modification time with the current time.
func TouchBlock(hash string) error {
	for _, vol := range KeepVM.Volumes() {
		err := vol.Touch(hash)
		if os.IsNotExist(err) {
			continue
		}
		// either err is nil (meaning success) or some error other
		// than "file does not exist" (which we should return upward).
		return err
	}
	// If we got here, the block was not found on any volume.
	return os.ErrNotExist
}

// IsValidLocator
//     Return true if the specified string is a valid Keep locator.
//     When Keep is extended to support hash types other than MD5,
//     this should be updated to cover those as well.
//
func IsValidLocator(loc string) bool {
	match, err := regexp.MatchString(`^[0-9a-f]{32}$`, loc)
	if err == nil {
		return match
	}
	log.Printf("IsValidLocator: %s\n", err)
	return false
}

// GetApiToken returns the OAuth2 token from the Authorization
// header of a HTTP request, or an empty string if no matching
// token is found.
func GetApiToken(req *http.Request) string {
	if auth, ok := req.Header["Authorization"]; ok {
		if pat, err := regexp.Compile(`^OAuth2\s+(.*)`); err != nil {
			log.Println(err)
		} else if match := pat.FindStringSubmatch(auth[0]); match != nil {
			return match[1]
		}
	}
	return ""
}

// IsExpired returns true if the given Unix timestamp (expressed as a
// hexadecimal string) is in the past, or if timestamp_hex cannot be
// parsed as a hexadecimal string.
func IsExpired(timestamp_hex string) bool {
	ts, err := strconv.ParseInt(timestamp_hex, 16, 0)
	if err != nil {
		log.Printf("IsExpired: %s\n", err)
		return true
	}
	return time.Unix(ts, 0).Before(time.Now())
}

// CanDelete returns true if the user identified by api_token is
// allowed to delete blocks.
func CanDelete(api_token string) bool {
	if api_token == "" {
		return false
	}
	// Blocks may be deleted only when Keep has been configured with a
	// data manager.
	if data_manager_token == "" {
		return false
	}
	if api_token == data_manager_token {
		return true
	}
	// TODO(twp): look up api_token with the API server
	// return true if is_admin is true and if the token
	// has unlimited scope
	return false
}
