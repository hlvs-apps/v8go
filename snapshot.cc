#include "snapshot.h"
#include "v8go.h"
#include "context-macros.h"
#include <atomic>
#include <cstring>
#include <mutex>
#include <memory>
#include <shared_mutex>
using namespace v8;
extern ArrayBuffer::Allocator* default_allocator;
extern std::shared_mutex isolate_creation_mutex;
extern "C" {
SnapshotResult SnapshotCreate(SnapshotScriptInput* scripts, int count) {
  SnapshotResult out = {};
  // Snapshot creators can extend and seal the default group's shared read-only
  // heap. No other creator or new isolate may join it before finalization.
  // Existing isolates already imply a finalized read-only heap.
  std::unique_lock<std::shared_mutex> creation_lock(isolate_creation_mutex);
  Isolate::CreateParams params;
  params.array_buffer_allocator = default_allocator;
  // These archives share one read-only heap and disable custom RO snapshots.
  // Initialize and retain the default sealed RO heap before the creator joins
  // the group. This keeps application objects in the snapshot's mutable heap
  // sections and lets the result coexist with ordinary isolates and other apps.
  auto dispose = [](Isolate* isolate) { if (isolate) isolate->Dispose(); };
  std::unique_ptr<Isolate, decltype(dispose)> baseline(Isolate::New(params), dispose);
  if (!baseline) { out.error.msg = strdup("could not initialize snapshot base heap"); return out; }
  SnapshotCreator creator(params);
  Isolate* iso = creator.GetIsolate();
  {
    HandleScope handles(iso);
    creator.SetDefaultContext(Context::New(iso));
    Local<Context> ctx = Context::New(iso);
    Context::Scope entered(ctx);
    TryCatch caught(iso);
    std::vector<Local<Value>> captures;
    iso->SetMicrotasksPolicy(MicrotasksPolicy::kExplicit);
    for (int i = 0; i < count; ++i) {
      auto& input = scripts[i];
      auto src = String::NewFromUtf8(iso, input.source).ToLocalChecked();
      auto origin = String::NewFromUtf8(iso, input.origin).ToLocalChecked();
      ScriptOrigin script_origin(origin);
      auto cache = input.cache_len ? new ScriptCompiler::CachedData(input.cache, input.cache_len) : nullptr;
      ScriptCompiler::Source source(src, script_origin, cache);
      Local<Script> script;
      Local<Value> result;
      if (!ScriptCompiler::Compile(ctx, &source, cache ? ScriptCompiler::kConsumeCodeCache : ScriptCompiler::kNoCompileOptions).ToLocal(&script)) {
        out.error = ExceptionError(caught, iso, ctx);
        return out;
      }
      if (cache && cache->rejected) {
        out.error.msg = strdup("V8 rejected snapshot script code cache");
        return out;
      }
      if (!script->Run(ctx).ToLocal(&result)) {
        out.error = ExceptionError(caught, iso, ctx);
        return out;
      }
      for (int j = 0; j < input.capture_count; ++j) {
        auto key = String::NewFromUtf8(iso, input.captures[j]).ToLocalChecked();
        Local<Value> value;
        if (!ctx->Global()->HasOwnProperty(ctx, key).FromMaybe(false) ||
            !ctx->Global()->Get(ctx, key).ToLocal(&value) ||
            !ctx->Global()->Delete(ctx, key).FromMaybe(false)) {
          out.error.msg = strdup("snapshot capture must name a removable own global property");
          return out;
        }
        // SnapshotData reads captures back by ordinal; V8 documents AddData's
        // return value as the index, so refuse to build if they ever diverge.
        if (creator.AddData(ctx, value) != captures.size()) {
          out.error.msg = strdup("V8 assigned an unexpected snapshot capture index");
          return out;
        }
        captures.push_back(value);
        ++out.captures;
      }
      iso->PerformMicrotaskCheckpoint();
      if (caught.HasCaught()) { out.error = ExceptionError(caught, iso, ctx); return out; }
      for (int j = 0; j < input.validator_count; ++j) {
        int index = input.validators[j];
        if (index < 0 || static_cast<size_t>(index) >= captures.size() || !captures[index]->IsFunction()) {
          out.error.msg = strdup("snapshot validator must name an existing captured function");
          return out;
        }
        if (!captures[index].As<Function>()->Call(ctx, Undefined(iso), 0, nullptr).ToLocal(&result)) {
          out.error = ExceptionError(caught, iso, ctx);
          return out;
        }
        if (result->IsPromise()) {
          out.error.msg = strdup("snapshot validators must be synchronous");
          return out;
        }
        // A synchronous return can still enqueue Promise jobs. Settle those
        // before the next validator/script and before serializing the context.
        iso->PerformMicrotaskCheckpoint();
        if (caught.HasCaught()) { out.error = ExceptionError(caught, iso, ctx); return out; }
      }
    }
    creator.AddContext(ctx);
  }
  StartupData blob = creator.CreateBlob(SnapshotCreator::FunctionCodeHandling::kKeep);
  out.data = blob.data;
  out.length = blob.raw_size;
  if (!out.data) out.error.msg = strdup("V8 could not create snapshot");
  return out;
}
void SnapshotDelete(const char* data) { delete[] data; }
static std::atomic<long> live_snapshot_blobs{0};
SnapshotBlobPtr SnapshotBlobNew(const char* data, int length) {
  auto owned = new char[length];
  memcpy(owned, data, length);
  live_snapshot_blobs.fetch_add(1);
  return new SnapshotBlob{{owned, length}};
}
void SnapshotBlobDelete(SnapshotBlobPtr blob) {
  delete[] blob->startup.data;
  delete blob;
  live_snapshot_blobs.fetch_sub(1);
}
long SnapshotBlobLiveCount() { return live_snapshot_blobs.load(); }
ContextPtr SnapshotNewContext(IsolatePtr iso, int index, int ref) {
  Locker lock(iso);
  Isolate::Scope entered(iso);
  HandleScope handles(iso);
  Local<Context> local;
  if (!Context::FromSnapshot(iso, index).ToLocal(&local)) return nullptr;
  local->SetEmbedderData(1, Integer::New(iso, ref));
  auto ctx = new m_ctx{};
  ctx->iso = iso;
  ctx->ptr.Reset(iso, local);
  return ctx;
}
ValuePtr SnapshotContextData(ContextPtr ctx, int index) {
  LOCAL_CONTEXT(ctx);
  Local<Value> value;
  if (!local_ctx->GetDataFromSnapshotOnce<Value>(index).ToLocal(&value)) return nullptr;
  auto result = new m_value{};
  result->iso = iso;
  result->ctx = ctx;
  result->ptr.Reset(iso, value);
  return tracked_value(ctx, result);
}
uint32_t SnapshotCacheVersionTag() { return ScriptCompiler::CachedDataVersionTag(); }
}
