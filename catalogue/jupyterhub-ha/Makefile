# Build / validate / publish targets for the jupyterhub-ha catalogue
# plugin. Intentionally tiny — the real CI lives in the
# openweft/jupyterhub-ha repo where the controller image is
# published ; this Makefile is for local dev + the framework's
# `weft plugin validate` smoke test.

IMAGE     ?= ghcr.io/openweft/jupyterhub-ha
TAG       ?= 0.1.0
PYTHON    ?= python3

.PHONY: all image validate compile lint clean

all: validate

# Build the controller image. Tag it twice — `:0.1.0` for
# reproducibility, `:latest` for the curious operator running it
# locally.
image:
	docker build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

# `validate` is the catch-all that mirrors what the framework
# agent's `weft plugin validate` will eventually call : every
# Python file py_compiles, the plugin.hcl parses, the markdown
# isn't obviously broken.
validate: compile
	@echo "== plugin.hcl exists =="
	@test -s plugin.hcl
	@echo "== docs/catalogue/jupyterhub-ha.md exists =="
	@test -s ../../docs/catalogue/jupyterhub-ha.md

# Pure-Python compile gate. No third-party deps required — the
# spawner guards `from jupyterhub.spawner import Spawner` so this
# works in a vanilla CI runner.
compile:
	$(PYTHON) -m py_compile spawner/weft_spawner.py jupyterhub_config.py

# `make lint` is here as a stub for the framework agent — once
# they wire `weft plugin lint` we replace this with a real call.
lint:
	@echo "lint : delegated to \`weft plugin validate catalogue/jupyterhub-ha\` (framework agent)"

clean:
	rm -rf spawner/__pycache__ __pycache__
