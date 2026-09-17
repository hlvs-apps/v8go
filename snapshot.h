#ifndef V8GO_SNAPSHOT_H
#define V8GO_SNAPSHOT_H
#include "context.h"
#ifdef __cplusplus
#include "deps/include/v8-snapshot.h"
// One native copy per Snapshot, shared by every isolate restored from it. The
// Go Snapshot counts its live isolates and deletes the copy after the last one
// is disposed.
struct SnapshotBlob {
  v8::StartupData startup;
};
extern "C" {
#else
typedef struct SnapshotBlob SnapshotBlob;
#endif
typedef SnapshotBlob* SnapshotBlobPtr;
typedef struct { const char* source; const char* origin; const unsigned char* cache; int cache_len; const char** captures; int capture_count; const int* validators; int validator_count; } SnapshotScriptInput;
typedef struct { const char* data; int length; int captures; RtnError error; } SnapshotResult;
SnapshotResult SnapshotCreate(SnapshotScriptInput*, int);
void SnapshotDelete(const char*);
SnapshotBlobPtr SnapshotBlobNew(const char*, int);
void SnapshotBlobDelete(SnapshotBlobPtr);
long SnapshotBlobLiveCount();
IsolatePtr SnapshotNewIsolate(SnapshotBlobPtr, IsolateConstraintsPtr);
ContextPtr SnapshotNewContext(IsolatePtr, int, int);
ValuePtr SnapshotContextData(ContextPtr, int);
uint32_t SnapshotCacheVersionTag();
#ifdef __cplusplus
}
#endif
#endif
