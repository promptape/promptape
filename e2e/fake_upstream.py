"""A stand-in for the OpenAI and Anthropic APIs used by the end-to-end test.

It speaks the real wire formats (JSON and server-sent events) so the
official SDKs parse its responses exactly as they would the real thing,
without any network access or API spend. Every response embeds a hit
counter so the test can prove which responses came from a recording.
"""

import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HITS = 0
LOCK = threading.Lock()


def next_hit():
    global HITS
    with LOCK:
        HITS += 1
        return HITS


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # keep test output quiet
        pass

    def do_GET(self):
        if self.path == "/hits":
            with LOCK:
                return self._json(200, {"hits": HITS})
        self._json(404, {"error": "not found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        if "x-api-key" not in self.headers and "authorization" not in self.headers:
            return self._json(401, {"error": {"message": "missing credentials"}})
        n = next_hit()
        if self.path.endswith("/v1/messages"):
            return self._anthropic(body, n)
        if self.path.endswith("/v1/chat/completions"):
            return self._openai_chat(body, n)
        if self.path.endswith("/v1/embeddings"):
            return self._json(200, {
                "object": "list",
                "data": [{"object": "embedding", "index": 0, "embedding": [0.1 * n, 0.2, 0.3]}],
                "model": body.get("model"),
                "usage": {"prompt_tokens": 8, "total_tokens": 8},
            })
        self._json(404, {"error": {"message": "unknown path " + self.path}})

    # -- Anthropic ---------------------------------------------------------
    def _anthropic(self, body, n):
        model = body.get("model", "claude-opus-5-5")
        text = f"Recorded answer #{n} from {model}"
        usage = {"input_tokens": 1200, "output_tokens": 300,
                 "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
        msg = {
            "id": f"msg_{n:04d}", "type": "message", "role": "assistant", "model": model,
            "content": [{"type": "text", "text": text}],
            "stop_reason": "end_turn", "stop_sequence": None, "usage": usage,
        }
        if not body.get("stream"):
            return self._json(200, msg)
        start = dict(msg, content=[], stop_reason=None, usage=dict(usage, output_tokens=1))
        events = [
            ("message_start", {"type": "message_start", "message": start}),
            ("content_block_start", {"type": "content_block_start", "index": 0,
                                     "content_block": {"type": "text", "text": ""}}),
        ]
        for word in text.split(" "):
            events.append(("content_block_delta", {"type": "content_block_delta", "index": 0,
                                                   "delta": {"type": "text_delta", "text": word + " "}}))
        events += [
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta",
                               "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                               "usage": {"output_tokens": 300}}),
            ("message_stop", {"type": "message_stop"}),
        ]
        self._sse([f"event: {name}\ndata: {json.dumps(data)}" for name, data in events])

    # -- OpenAI ------------------------------------------------------------
    def _openai_chat(self, body, n):
        model = body.get("model", "gpt-5")
        text = f"Recorded answer #{n} from {model}"
        usage = {"prompt_tokens": 1200, "completion_tokens": 300, "total_tokens": 1500}
        created = 1763078400
        if not body.get("stream"):
            return self._json(200, {
                "id": f"chatcmpl-{n:04d}", "object": "chat.completion", "created": created, "model": model,
                "choices": [{"index": 0, "finish_reason": "stop",
                             "message": {"role": "assistant", "content": text}}],
                "usage": usage,
            })
        chunks = []
        base = {"id": f"chatcmpl-{n:04d}", "object": "chat.completion.chunk", "created": created, "model": model}
        chunks.append(dict(base, choices=[{"index": 0, "delta": {"role": "assistant", "content": ""},
                                           "finish_reason": None}]))
        for word in text.split(" "):
            chunks.append(dict(base, choices=[{"index": 0, "delta": {"content": word + " "},
                                               "finish_reason": None}]))
        chunks.append(dict(base, choices=[{"index": 0, "delta": {}, "finish_reason": "stop"}]))
        if (body.get("stream_options") or {}).get("include_usage"):
            chunks.append(dict(base, choices=[], usage=usage))
        self._sse([f"data: {json.dumps(c)}" for c in chunks] + ["data: [DONE]"])

    # -- helpers -----------------------------------------------------------
    def _json(self, status, obj):
        raw = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _sse(self, events):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream; charset=utf-8")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for ev in events:
            time.sleep(0.01)
            self.wfile.write((ev + "\n\n").encode())
            self.wfile.flush()
        self.close_connection = True


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 4010
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
