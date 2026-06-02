"""
Thin wrapper around LiteLLM proxy that adds:
  - response_cost on streaming usage chunks (computed via litellm.completion_cost())
  - Model capability metadata exposed on /v1/models (mode, supports_vision, etc.)
Every endpoint (chat, embeddings, images, audio) is routed through the LiteLLM
Router so provider-specific request/response translation and auth are handled by
LiteLLM. The server (Go) only ever talks to this proxy — no provider URLs or API
keys live in Go.
"""

import sys
import json
import base64
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
# model_to_info: model_name -> the full model_info dict (for /v1/models surfacing)
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
        # We only expose capability flags — never api keys or other proxy-internal config.
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


_SPEECH_MEDIA_TYPES = {
    "mp3": "audio/mpeg",
    "opus": "audio/opus",
    "aac": "audio/aac",
    "flac": "audio/flac",
    "wav": "audio/wav",
    "pcm": "audio/pcm",
}


async def _inline_image_urls(data):
    """Fetch any URL-only image results and add them as base64.

    Some providers (e.g. fal.ai) ignore response_format and always return a
    hosted image URL instead of base64. The Go server only talks to this proxy
    and expects b64_json, so we normalize the shape here.
    """
    missing = [d for d in data if isinstance(d, dict) and not d.get("b64_json") and d.get("url")]
    if not missing:
        return
    async with httpx.AsyncClient(timeout=180.0) as client:
        for d in missing:
            try:
                r = await client.get(d["url"])
                r.raise_for_status()
                d["b64_json"] = base64.b64encode(r.content).decode("ascii")
            except Exception as e:
                logger.warning(f"Could not fetch image url {d.get('url')!r}: {e}")


@app.post("/v1/images/generations")
async def images_generations(request: Request):
    body = await request.json()
    model = body.pop("model", "")
    prompt = body.pop("prompt", "")
    response_format = body.get("response_format")

    response = await router.aimage_generation(prompt=prompt, model=model, **body)
    result = response.model_dump()

    if response_format == "b64_json":
        await _inline_image_urls(result.get("data") or [])

    return JSONResponse(content=result)


@app.post("/v1/audio/speech")
async def audio_speech(request: Request):
    body = await request.json()
    model = body.pop("model", "")
    input_text = body.pop("input", "")
    voice = body.pop("voice", "")
    response_format = body.get("response_format", "mp3")

    response = await router.aspeech(model=model, input=input_text, voice=voice, **body)
    # litellm returns an HttpxBinaryResponseContent wrapper; .content is the raw audio bytes.
    return Response(
        content=response.content,
        media_type=_SPEECH_MEDIA_TYPES.get(response_format, "audio/mpeg"),
    )


@app.post("/v1/audio/transcriptions")
async def audio_transcriptions(request: Request):
    # Multipart form. The model arrives as a form field named "model"; the audio as "file".
    form = await request.form()
    model = (form.get("model") or "").strip()
    upload = form.get("file")
    if upload is None or not hasattr(upload, "read"):
        return JSONResponse(status_code=400, content={"error": "missing 'file' in form data"})

    file_bytes = await upload.read()
    filename = upload.filename or "audio"

    # Forward the standard optional transcription params if present.
    kwargs = {}
    for key in ("language", "prompt", "response_format", "temperature", "timestamp_granularities"):
        value = form.get(key)
        if value is not None:
            kwargs[key] = value

    response = await router.atranscription(file=(filename, file_bytes), model=model, **kwargs)
    # response_format=text/srt/vtt yields a plain string; JSON formats yield an object.
    if isinstance(response, str):
        return Response(content=response, media_type="text/plain")
    return JSONResponse(content=response.model_dump())


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
