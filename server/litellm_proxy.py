"""
Thin wrapper around LiteLLM proxy that adds response_cost to streaming usage chunks.
Uses litellm.completion_cost() to compute accurate cost from LiteLLM's pricing database.
"""

import sys
import json
import logging
import litellm
from litellm import Router
from fastapi import FastAPI, Request
from fastapi.responses import StreamingResponse, JSONResponse
import uvicorn
import yaml
import os

logger = logging.getLogger("litellm_proxy")

app = FastAPI()
router = None
# Map from short model_name to the full litellm model string (e.g. "anthropic/claude-sonnet-4-6")
model_to_litellm = {}


def load_config(config_path):
    with open(config_path) as f:
        config = yaml.safe_load(f)

    model_list = config.get("model_list", [])

    # Resolve os.environ/ references in api_key fields and build model mapping
    for model in model_list:
        params = model.get("litellm_params", {})
        api_key = params.get("api_key", "")
        if isinstance(api_key, str) and api_key.startswith("os.environ/"):
            env_var = api_key.replace("os.environ/", "")
            params["api_key"] = os.environ.get(env_var, "")

        # Track the mapping from short name to full litellm model name
        model_to_litellm[model["model_name"]] = params.get("model", model["model_name"])

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
        models.append({
            "id": deployment["model_name"],
            "object": "model",
            "owned_by": "plurality",
        })
    return {"object": "list", "data": models}


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
