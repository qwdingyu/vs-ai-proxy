#!/usr/bin/env python3
"""
真实二进制端到端核查（发布门槛）

为什么需要这个脚本
------------------
单元测试与 `go test` 验证的是**代码**，而本脚本验证的是**构建产物**：把真正要
发布的二进制跑起来，走完 VS Copilot BYOM 的完整链路。历史教训：

  2026-09 max_tokens 事故中，代码层测试全绿、管理页也能测通，但真实二进制
  会把「输出能力上限」当作 max_tokens 发给上游（实测 131072），导致 VS Copilot
  完全不可用。这类缺陷只有跑真实产物才能拦住。

核查链路
--------
  1. 就绪        /health
  2. 发现        /api/tags、/api/show、/v1/models 暴露的能力元数据
  3. 路由        /api/tags 暴露出的**每一个**标识都能路由到上游
  4. 请求正确性  能力上限不得被当作 max_tokens 生成；客户端声明值必须原样送达
  5. 协议一致性  /api/chat 只接受 options 嵌套形态（顶层能力字段不得变成生成参数）
  6. 流式工具调用 OpenAI SSE 工具调用增量 → Ollama tool_calls + done:true
  7. OpenAI 入口 /v1/chat/completions 可用且参数正确

隔离性
------
  · XDG_CONFIG_HOME 指向临时目录 → 使用独立 config.json，绝不触碰用户真实配置
  · 上游是本地 mock，不访问任何真实 provider，不需要 API Key
  · 端口自动选取空闲端口，可与正在运行的实例共存
  · 结束时无论成败都清理进程与临时目录

用法
----
  python3 tests/e2e_binary_check.py --binary ./vs-ai-proxy
  python3 tests/e2e_binary_check.py --binary ./dist/xxx.exe --verbose
  make e2e-check          # 自动 build 后执行
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# ---------------------------------------------------------------------------
# 输出编码：Windows 控制台默认使用 cp1252 等本地代码页，**无法编码中文**。
# 本脚本的报告含中文，若不做处理会在 Windows 上抛
#   UnicodeEncodeError: 'charmap' codec can't encode characters
# 使核查**在全部通过后**仍以 exit 1 结束（假失败），进而阻塞发布。
# 这里在脚本内强制 UTF-8 并对无法编码的字符降级，不依赖调用方设置
# PYTHONIOENCODING，本地与 CI 行为一致。
# ---------------------------------------------------------------------------
for _stream in (sys.stdout, sys.stderr):
    try:
        _stream.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError, OSError):
        pass  # 流被替换或不支持重配置时保持原样

# ---------------------------------------------------------------------------
# 常量：复刻线上真实形态（能力上限很大、max_tokens 未配置）
# ---------------------------------------------------------------------------
MODEL_NAME = "e2e-model"
PROVIDER_ID = "mock"
CONTEXT_LENGTH = 1048576
MAX_OUTPUT_TOKENS = 131072
CLIENT_NUM_PREDICT = 4096

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "get_file",
            "description": "read a file",
            "parameters": {
                "type": "object",
                "properties": {"path": {"type": "string"}},
            },
        },
    }
]

# 请求里带这个标记时，mock 上游返回工具调用增量（确定性触发，不靠计数器）
TOOL_TRIGGER = "USE_TOOL"


def free_port() -> int:
    """让内核分配一个空闲端口，避免与正在运行的实例冲突。"""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class MockUpstream(BaseHTTPRequestHandler):
    """最小 OpenAI 兼容上游：记录收到的原始请求体，并按需返回 SSE。"""

    captured: list[str] = []
    lock = threading.Lock()

    def log_message(self, *args):  # 静默，避免污染输出
        pass

    def _read_body(self) -> str:
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n).decode("utf-8", "replace") if n else ""

    def _json(self, payload: dict, status: int = 200) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.endswith("/models"):
            self._json({"data": [{"id": MODEL_NAME}]})
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        raw = self._read_body()
        with MockUpstream.lock:
            MockUpstream.captured.append(raw)

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        if TOOL_TRIGGER in raw:
            # 工具调用增量分两片发出，验证代理能正确聚合
            self.wfile.write(
                b'data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":'
                b'[{"index":0,"id":"call_1","type":"function","function":'
                b'{"name":"get_file","arguments":"{\\"path\\":"}}]}}]}\n\n'
                b'data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,'
                b'"function":{"arguments":"\\"a.go\\"}"}}]}}]}\n\n'
                b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}\n\n'
                b"data: [DONE]\n\n"
            )
        else:
            self.wfile.write(
                b'data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}\n\n'
                b"data: [DONE]\n\n"
            )


class Checker:
    def __init__(self, verbose: bool):
        self.verbose = verbose
        self.results: list[tuple[str, bool, str]] = []

    def check(self, name: str, ok: bool, detail: object = "") -> bool:
        text = str(detail)
        if len(text) > 200:
            text = text[:200] + "…"
        self.results.append((name, bool(ok), text))
        return bool(ok)

    def report(self) -> int:
        print("\n" + "=" * 72)
        print("真实二进制端到端核查结果")
        print("=" * 72)
        for name, ok, detail in self.results:
            mark = "PASS" if ok else "FAIL"
            print(f"  [{mark}] {name}")
            if not ok:
                print(f"         → {detail}")
            elif self.verbose and detail:
                print(f"         {detail}")
        passed = sum(1 for _, ok, _ in self.results if ok)
        total = len(self.results)
        print("=" * 72)
        print(f"{'✅ 通过' if passed == total else '❌ 失败'}  ({passed}/{total})")
        return 0 if passed == total else 1


def http_json(url: str, payload: dict | None = None, timeout: float = 30.0):
    """有 body 默认 POST，无 body 默认 GET（避免误用 GET 带 body 触发 405）。"""
    method = "POST" if payload is not None else "GET"
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(
        url, data=data, method=method, headers={"Content-Type": "application/json"}
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def wait_ready(url: str, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=1):
                return True
        except Exception:
            time.sleep(0.2)
    return False


def wait_capture(count: int, timeout: float = 10.0) -> list[str]:
    """轮询等待上游捕获到至少 count 条请求，替代固定 sleep。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        with MockUpstream.lock:
            if len(MockUpstream.captured) >= count:
                return list(MockUpstream.captured)
        time.sleep(0.05)
    with MockUpstream.lock:
        return list(MockUpstream.captured)


def clear_capture() -> None:
    with MockUpstream.lock:
        MockUpstream.captured.clear()


def write_config(config_dir: str, proxy_port: int, mock_port: int) -> None:
    cfg = {
        "config_version": 2,
        "port": proxy_port,
        "default_model": MODEL_NAME,
        "providers": [
            {
                "id": PROVIDER_ID,
                "name": PROVIDER_ID,
                "display_name": PROVIDER_ID,
                "api_key": "e2e-test-key",
                "base_url": f"http://127.0.0.1:{mock_port}/v1",
                "type": "openai",
                "enabled": True,
                "priority": 0,
                "transport": {
                    "chat_path": "chat/completions",
                    "models_path": "models",
                },
            }
        ],
        "models": [
            {
                "name": MODEL_NAME,
                "provider_id": PROVIDER_ID,
                "provider": PROVIDER_ID,
                "context_length": CONTEXT_LENGTH,
                "max_output_tokens": MAX_OUTPUT_TOKENS,
                "supports_tools": True,
                "supports_vision": False,
                "enabled": True,
            }
        ],
    }
    os.makedirs(os.path.join(config_dir, "vs-ai-proxy"), exist_ok=True)
    with open(os.path.join(config_dir, "vs-ai-proxy", "config.json"), "w") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)


def run_checks(base: str, c: Checker) -> None:
    # ---------- 1. 发现：/api/tags ----------
    status, body = http_json(f"{base}/api/tags")
    tags = json.loads(body) if status == 200 else {}
    models = tags.get("models") or []
    c.check("发现: GET /api/tags 200 且列出模型", status == 200 and bool(models), f"HTTP {status}")
    if not models:
        return
    m = models[0]

    c.check("发现: tags 暴露 context_length", m.get("context_length") == CONTEXT_LENGTH, m.get("context_length"))
    c.check("发现: tags 暴露 max_output_tokens", m.get("max_output_tokens") == MAX_OUTPUT_TOKENS, m.get("max_output_tokens"))
    c.check("发现: tags 暴露 input_token_limit", m.get("input_token_limit") == CONTEXT_LENGTH, m.get("input_token_limit"))
    c.check("发现: tags 标记 supports_tools", m.get("supports_tools") is True, m.get("supports_tools"))

    display_name = m.get("name") or MODEL_NAME
    status, body = http_json(f"{base}/api/show?model=" + urllib.parse.quote(display_name))
    show = json.loads(body) if status == 200 else {}
    c.check("发现: GET /api/show 200", status == 200, f"HTTP {status}")
    c.check("发现: show 暴露 context_length", show.get("context_length") == CONTEXT_LENGTH, show.get("context_length"))
    c.check("发现: show 暴露 max_output_tokens", show.get("max_output_tokens") == MAX_OUTPUT_TOKENS, show.get("max_output_tokens"))

    status, body = http_json(f"{base}/v1/models")
    ids = [x.get("id") for x in (json.loads(body).get("data") or [])] if status == 200 else []
    c.check(
        "发现: GET /v1/models 列出模型",
        status == 200 and any(MODEL_NAME in (i or "") for i in ids),
        f"HTTP {status} ids={ids[:3]}",
    )

    # ---------- 2. 路由：tags 暴露的每个标识都必须可路由 ----------
    identifiers = [x for x in [m.get("name"), m.get("model")] + list(m.get("aliases") or []) if x]
    routed = []
    for ident in identifiers:
        clear_capture()
        status, _ = http_json(
            f"{base}/api/chat",
            {"model": ident, "messages": [{"role": "user", "content": "hi"}], "stream": False},
        )
        got = wait_capture(1, timeout=5.0)
        if status == 200 and got:
            routed.append(ident)
    c.check(
        f"路由: /api/tags 暴露的 {len(identifiers)} 个标识全部可路由",
        len(routed) == len(identifiers) and bool(identifiers),
        f"成功 {len(routed)}/{len(identifiers)}；失败={[i for i in identifiers if i not in routed]}",
    )

    # ---------- 3. 请求正确性：能力上限不得变成 max_tokens ----------
    clear_capture()
    status, _ = http_json(
        f"{base}/api/chat",
        {
            "model": MODEL_NAME,
            "messages": [{"role": "user", "content": "hi"}],
            "stream": True,
            "tools": TOOLS,
        },
    )
    captured = wait_capture(1)
    sent = json.loads(captured[-1]) if captured else {}
    c.check(
        "请求: 带 tools+流式、客户端未传上限 → 上游不得收到 max_tokens",
        status == 200 and captured and "max_tokens" not in sent,
        captured[-1] if captured else f"HTTP {status} 上游未被调用",
    )
    c.check(
        "请求: 上游不得收到 num_ctx（内部字段不得外泄）",
        "num_ctx" not in sent,
        captured[-1] if captured else "",
    )

    # ---------- 4. 客户端显式声明必须原样送达 ----------
    clear_capture()
    status, _ = http_json(
        f"{base}/api/chat",
        {
            "model": MODEL_NAME,
            "messages": [{"role": "user", "content": "hi"}],
            "stream": True,
            "tools": TOOLS,
            "options": {"num_predict": CLIENT_NUM_PREDICT},
        },
    )
    captured = wait_capture(1)
    sent = json.loads(captured[-1]) if captured else {}
    c.check(
        f"请求: 客户端声明 options.num_predict={CLIENT_NUM_PREDICT} → 上游原样收到",
        sent.get("max_tokens") == CLIENT_NUM_PREDICT,
        captured[-1] if captured else f"HTTP {status} 上游未被调用",
    )

    # ---------- 5. 协议一致性：顶层能力字段不得变成生成参数 ----------
    clear_capture()
    status, _ = http_json(
        f"{base}/api/chat",
        {
            "model": MODEL_NAME,
            "messages": [{"role": "user", "content": "hi"}],
            "stream": True,
            "tools": TOOLS,
            "max_tokens": CLIENT_NUM_PREDICT,          # 非 Ollama schema
            "max_output_tokens": MAX_OUTPUT_TOKENS,    # 非 Ollama schema
        },
    )
    captured = wait_capture(1)
    sent = json.loads(captured[-1]) if captured else {}
    c.check(
        "协议: /api/chat 顶层 max_tokens/max_output_tokens 不得成为生成参数",
        "max_tokens" not in sent,
        captured[-1] if captured else f"HTTP {status} 上游未被调用",
    )

    # ---------- 6. 流式工具调用 ----------
    status, body = http_json(
        f"{base}/api/chat",
        {
            "model": MODEL_NAME,
            "messages": [{"role": "user", "content": f"read a.go {TOOL_TRIGGER}"}],
            "stream": True,
            "tools": TOOLS,
        },
    )
    c.check(
        "流式: 工具调用增量聚合为 Ollama tool_calls",
        status == 200 and '"tool_calls"' in body and '"get_file"' in body and "a.go" in body,
        body,
    )
    c.check("流式: 含终止帧 done:true", '"done":true' in body, body[-160:])
    c.check(
        "流式: 工具参数不得泄漏进 content",
        '"content":"{\\"path\\"' not in body,
        body[:160],
    )

    # ---------- 7. OpenAI 兼容入口 ----------
    clear_capture()
    status, _ = http_json(
        f"{base}/v1/chat/completions",
        {
            "model": MODEL_NAME,
            "messages": [{"role": "user", "content": "hi"}],
            "stream": True,
            "tools": TOOLS,
        },
    )
    captured = wait_capture(1)
    sent = json.loads(captured[-1]) if captured else {}
    c.check(
        "OpenAI 入口: /v1/chat/completions 可用且不得生成 max_tokens",
        status == 200 and captured and "max_tokens" not in sent,
        captured[-1] if captured else f"HTTP {status} 上游未被调用",
    )


MAX_LAUNCH_ATTEMPTS = 3


def stop(proc) -> None:
    """跨平台终止子进程。

    不要直接用 `signal.SIGKILL`：Windows 上 Python 的 signal 模块**不定义**该常量，
    会抛 AttributeError。`proc.kill()` 在 Windows 走 TerminateProcess、在 Unix 走
    SIGKILL，语义正确且跨平台。
    """
    if proc is None or proc.poll() is not None:
        return
    try:
        proc.kill()
        proc.wait(timeout=5)
    except Exception:
        pass


def read_log(path: str, limit: int = 3000) -> str:
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()[-limit:]
    except OSError:
        return "(无法读取代理日志)"


def launch(binary: str, temp_dir: str, ready_timeout: float):
    """启动 mock 上游与代理，返回 (proc, mock, base, log_path)；未就绪时前两项为 None。

    每次都重新选取空闲端口：`free_port()` 是「先绑后关」，理论上存在端口在关闭与
    代理绑定之间被抢占的窗口，因此启动失败由调用方换端口重试。
    """
    proxy_port = free_port()
    mock_port = free_port()
    write_config(temp_dir, proxy_port, mock_port)

    mock = ThreadingHTTPServer(("127.0.0.1", mock_port), MockUpstream)
    threading.Thread(target=mock.serve_forever, daemon=True).start()

    log_path = os.path.join(temp_dir, "proxy.log")
    log_file = open(log_path, "w", encoding="utf-8")
    env = dict(os.environ)
    env["XDG_CONFIG_HOME"] = temp_dir
    env["VS_AI_PROXY_AUTO_UPDATE"] = "false"  # 核查不应触发任何网络更新

    # 关键：stdout 重定向到**文件**而不是 PIPE。
    # 用 PIPE 且从不读取时，一旦代理日志写满管道缓冲（约 64KB），代理会阻塞在
    # 写日志上，导致请求挂起、门槛假失败。重定向到文件既无死锁，又便于失败时回读。
    proc = subprocess.Popen(
        [binary],
        env=env,
        stdout=log_file,
        stderr=subprocess.STDOUT,
        text=True,
    )
    base = f"http://127.0.0.1:{proxy_port}"
    if wait_ready(f"{base}/health", ready_timeout):
        return proc, mock, base, log_path

    stop(proc)
    mock.shutdown()
    return None, None, None, log_path


def main() -> int:
    parser = argparse.ArgumentParser(description="真实二进制端到端核查（发布门槛）")
    parser.add_argument("--binary", required=True, help="待核查的可执行文件路径")
    parser.add_argument("--verbose", action="store_true", help="打印每项详情")
    parser.add_argument("--keep-temp", action="store_true", help="保留临时目录以便排查")
    parser.add_argument("--ready-timeout", type=float, default=30.0, help="等待服务就绪的秒数")
    args = parser.parse_args()

    binary = os.path.abspath(args.binary)
    if not os.path.isfile(binary):
        print(f"❌ 找不到可执行文件: {binary}", file=sys.stderr)
        return 2
    if not os.access(binary, os.X_OK):
        print(f"❌ 可执行文件没有执行权限: {binary}", file=sys.stderr)
        return 2

    temp_dir = tempfile.mkdtemp(prefix="vs-ai-proxy-e2e-")
    checker = Checker(args.verbose)
    proc = None
    mock = None
    log_path = os.path.join(temp_dir, "proxy.log")
    exit_code = 1

    try:
        base = None
        for attempt in range(1, MAX_LAUNCH_ATTEMPTS + 1):
            proc, mock, base, log_path = launch(binary, temp_dir, args.ready_timeout)
            if base:
                break
            if attempt < MAX_LAUNCH_ATTEMPTS:
                print(f"⚠ 第 {attempt} 次启动未就绪，换端口重试…", file=sys.stderr)

        if not base:
            print(
                f"❌ 代理在 {MAX_LAUNCH_ATTEMPTS} 次尝试内均未就绪，二进制输出如下：\n"
                + read_log(log_path),
                file=sys.stderr,
            )
            return 1

        run_checks(base, checker)
        exit_code = checker.report()
    finally:
        stop(proc)
        if mock is not None:
            mock.shutdown()
        if args.keep_temp:
            print(f"（临时目录保留: {temp_dir}）")
        else:
            shutil.rmtree(temp_dir, ignore_errors=True)

    return exit_code


if __name__ == "__main__":
    sys.exit(main())
