// Package procjob groups child processes into an OS job so that stopping a
// workload also stops everything it spawned.
//
// On Windows the members live in a Job Object with kill-on-last-close
// semantics: closing the job handle (or the death of the process that owned
// the only handle) terminates every member and every descendant that was
// spawned inside the job. On other platforms the implementation is a no-op
// stub, mirroring how affinity degrades.
package procjob
