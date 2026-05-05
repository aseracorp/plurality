"""
Thin wrapper around LiteLLM proxy that adds:
  - response_cost on streaming usage chunks (computed via litellm.completion_cost())
  - Model capability metadata exposed on /v1/models (mode, supports_vision, etc.)
  - Generic provider passthrough for /v1/images/generations, /v1/audio/speech,
    /v1/audio/transcriptions, driven by `model_info.endpoint_url` in the config.
The server (Go) only ever talks to this proxy — no provider URLs or API keys live in Go.
"""

import sys
import json
import logging
import litellm
from litellm import Router
from fastapi import FastAPI, Request
from fastapi.responses import StreamingResponse, JSONResponse, Response
import uvicorn
import yaml
import os
import httpx

logger = logging.getLogger("litellm_proxy")

app = FastAPI()
router = None
# Map from short model_name to the full litellm model string (e.g. "anthropic/claude-sonnet-4-6")
model_to_litellm = {}
# Per-model passthrough routing for non-chat endpoints.
#   model_to_endpoint: model_name -> upstream URL (from model_info.endpoint_url)
#   model_to_api_key:  model_name -> resolved upstream API key (from litellm_params.api_key)
#   model_to_info:     model_name -> the full model_info dict (for /v1/models surfacing)
model_to_endpoint = {}
model_to_api_key = {}
model_to_info = {}


def load_config(config_path):
    with open(config_path) as f:
        config = yaml.safe_load(f)

    model_list = config.get("model_list", [])

    for entry in model_list:
        params = entry.get("litellm_params", {})
        api_key = params.get("api_key", "")
        if isinstance(api_key, str) and api_key.startswith("os.environ/"):
            env_var = api_key.replace("os.environ/", "")
            params["api_key"] = os.environ.get(env_var, "")

        name = entry["model_name"]
        info = entry.get("model_info", {}) or {}

        model_to_litellm[name] = params.get("model", name)
        model_to_info[name] = info
        if info.get("endpoint_url"):
            model_to_endpoint[name] = info["endpoint_url"]
            model_to_api_key[name] = params.get("api_key", "")

    return model_list


def get_cost(model_name, prompt_tokens, completion_tokens):
    """Compute cost using litellm's pricing database."""
    from litellm.utils import ModelResponse, Usage

    litellm_model = model_to_litellm.get(model_name, model_name)

    for try_model in [litellm_model, model_name] if litellm_model != model_name else [litellm_model]:
        try:
            mock_response = ModelResponse()
            mock_response.model = try_model
            mock_response.usage = Usage(
                prompt_tokens=prompt_tokens,
                completion_tokens=completion_tokens,
                total_tokens=prompt_tokens + completion_tokens,
            )
            cost = litellm.completion_cost(completion_response=mock_response)
            return cost
        except Exception as e:
            logger.warning(f"Could not compute cost for {try_model}: {e}")

    return None


@app.get("/health")
async def health():
    return {"status": "healthy"}


@app.get("/v1/models")
async def list_models():
    models = []
    for deployment in router.model_list:
        name = deployment["model_name"]
        info = deployment.get("model_info") or model_to_info.get(name) or {}
        entry = {
            "id": name,
            "object": "model",
            "owned_by": "plurality",
        }
        # Surface capability metadata so the Go server can drive its registry from this.
        # We deliberately omit endpoint_url / api_key — those are proxy-internal.
        for k in ("mode", "supports_vision", "supports_function_calling",
                  "supports_audio_input", "supports_audio_output"):
            if k in info:
                entry[k] = info[k]
        models.append(entry)
    return {"object": "list", "data": models}


@app.post("/v1/embeddings")
async def embeddings(request: Request):
    body = await request.json()
    model = body.get("model", "")
    input_text = body.get("input", "")

    response = await router.aembedding(model=model, input=[input_text] if isinstance(input_text, str) else input_text)
    return JSONResponse(content=response.model_dump())


@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    body = await request.json()
    model = body.get("model", "")
    stream = body.get("stream", False)
    messages = body.get("messages", [])
    tools = body.get("tools", None)
    temperature = body.get("temperature", None)
    max_tokens = body.get("max_tokens", None)
    stream_options = body.get("stream_options", None)

    kwargs = {
        "model": model,
        "messages": messages,
        "stream": stream,
    }
    if tools:
        kwargs["tools"] = tools
    if temperature is not None:
        kwargs["temperature"] = temperature
    if max_tokens is not None:
        kwargs["max_tokens"] = max_tokens
    if stream_options:
        kwargs["stream_options"] = stream_options

    if not stream:
        response = await router.acompletion(**kwargs)
        result = response.model_dump()

        usage = result.get("usage", {})
        if usage:
            cost = get_cost(model, usage.get("prompt_tokens", 0), usage.get("completion_tokens", 0))
            if cost is not None:
                result["usage"]["response_cost"] = cost

        return JSONResponse(content=result)

    # Streaming
    response = await router.acompletion(**kwargs)

    async def generate():
        collected_text = ""
        got_usage = False

        async for chunk in response:
            chunk_dict = chunk.model_dump()

            # Collect output text for fallback token counting
            choices = chunk_dict.get("choices", [])
            if choices:
                delta = choices[0].get("delta", {})
                if delta and delta.get("content"):
                    collected_text += delta["content"]

            # If this chunk has usage info, inject cost
            usage = chunk_dict.get("usage")
            if usage and (usage.get("prompt_tokens", 0) > 0 or usage.get("completion_tokens", 0) > 0):
                got_usage = True
                cost = get_cost(model, usage.get("prompt_tokens", 0), usage.get("completion_tokens", 0))
                if cost is not None:
                    chunk_dict["usage"]["response_cost"] = cost

            yield f"data: {json.dumps(chunk_dict)}\n\n"

        # If the provider didn't return usage (e.g. Fireworks), estimate tokens and inject a usage chunk
        if not got_usage and collected_text:
            litellm_model = model_to_litellm.get(model, model)
            try:
                prompt_tokens = litellm.token_counter(model=litellm_model, messages=messages)
            except Exception:
                prompt_tokens = sum(len(m.get("content", "")) for m in messages if isinstance(m.get("content"), str)) // 4
            try:
                completion_tokens = litellm.token_counter(model=litellm_model, text=collected_text)
            except Exception:
                completion_tokens = len(collected_text) // 4

            usage_chunk = {
                "id": "usage-fallback",
                "model": model,
                "object": "chat.completion.chunk",
                "choices": [],
                "usage": {
                    "prompt_tokens": prompt_tokens,
                    "completion_tokens": completion_tokens,
                    "total_tokens": prompt_tokens + completion_tokens,
                },
            }

            cost = get_cost(model, prompt_tokens, completion_tokens)
            if cost is not None:
                usage_chunk["usage"]["response_cost"] = cost

            logger.info(f"Provider didn't return usage for {model}, estimated: prompt={prompt_tokens} completion={completion_tokens}")
            yield f"data: {json.dumps(usage_chunk)}\n\n"

        yield "data: [DONE]\n\n"

    return StreamingResponse(
        generate(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )


def _resolve_passthrough(model: str, expected_mode: str):
    """Return (upstream_url, api_key) for a model, or raise if unknown / wrong mode."""
    info = model_to_info.get(model) or {}
    if info.get("mode") != expected_mode:
        raise ValueError(f"Model {model!r} is not configured with mode={expected_mode!r}")
    url = model_to_endpoint.get(model)
    key = model_to_api_key.get(model, "")
    if not url:
        raise ValueError(f"Model {model!r} has no endpoint_url in model_info")
    return url, key


@app.post("/v1/images/generations")
async def images_generations(request: Request):
    body = await request.json()
    model = body.get("model", "")
    try:
        upstream_url, upstream_key = _resolve_passthrough(model, "image_generation")
    except ValueError as e:
        return JSONResponse(status_code=400, content={"error": str(e)})

    headers = {"Content-Type": "application/json"}
    if upstream_key:
        headers["Authorization"] = f"Bearer {upstream_key}"

    async with httpx.AsyncClient(timeout=180.0) as client:
        upstream = await client.post(upstream_url, json=body, headers=headers)

    return Response(
        content=upstream.content,
        status_code=upstream.status_code,
        media_type=upstream.headers.get("content-type", "application/json"),
    )


@app.post("/v1/audio/speech")
async def audio_speech(request: Request):
    body = await request.json()
    model = body.get("model", "")
    try:
        upstream_url, upstream_key = _resolve_passthrough(model, "audio_speech")
    except ValueError as e:
        return JSONResponse(status_code=400, content={"error": str(e)})

    headers = {"Content-Type": "application/json"}
    if upstream_key:
        headers["Authorization"] = f"Bearer {upstream_key}"

    async def stream():
        async with httpx.AsyncClient(timeout=180.0) as client:
            async with client.stream("POST", upstream_url, json=body, headers=headers) as upstream:
                async for chunk in upstream.aiter_bytes():
                    if chunk:
                        yield chunk

    return StreamingResponse(
        stream(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )


@app.post("/v1/audio/transcriptions")
async def audio_transcriptions(request: Request):
    # Multipart form. The model arrives as a form field named "model".
    # Extract it without consuming the original raw body so we can forward bytes verbatim.
    form = await request.form()
    model = (form.get("model") or "").strip()
    try:
        upstream_url, upstream_key = _resolve_passthrough(model, "audio_transcription")
    except ValueError as e:
        return JSONResponse(status_code=400, content={"error": str(e)})

    # Re-build a multipart body for the upstream request from the parsed form.
    # We can't reuse the raw bytes because Starlette has already consumed them.
    files = {}
    data = {}
    for key, value in form.multi_items():
        if hasattr(value, "filename") and value.filename is not None:
            files[key] = (value.filename, await value.read(), value.content_type or "application/octet-stream")
        else:
            data[key] = value

    headers = {}
    if upstream_key:
        headers["Authorization"] = f"Bearer {upstream_key}"

    async with httpx.AsyncClient(timeout=180.0) as client:
        upstream = await client.post(upstream_url, headers=headers, data=data, files=files)

    return Response(
        content=upstream.content,
        status_code=upstream.status_code,
        media_type=upstream.headers.get("content-type", "application/json"),
    )


def main():
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    parser.add_argument("--port", type=int, default=4000)
    parser.add_argument("--host", default="127.0.0.1")
    args = parser.parse_args()

    global router
    model_list = load_config(args.config)
    router = Router(model_list=model_list)

    # Drop unsupported params silently
    litellm.drop_params = True

    logging.basicConfig(level=logging.INFO)

    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    main()
