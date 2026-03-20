# Flomation README Generator Prompt

You are generating a README.md for a Flomation project. Read the project files and produce a README that follows the Flomation standard template below.

## Rules

1. Be concise. No waffle. Every sentence should add value.
2. Use tables for inputs, outputs, configuration, and environment variables.
3. Include real-world usage examples — not abstract placeholder code.
4. If a `metadata.json` exists, use it as the primary source for module name, description, inputs, and outputs.
5. If a `package.json` exists, extract the project name, version, description, and dependencies from it.
6. If a `go.mod` exists, extract the module path and Go version from it.
7. If a `Dockerfile` exists, note the base image and any exposed ports.
8. If a `.gitlab-ci.yml` exists, summarise the CI/CD pipeline stages briefly.
9. Do NOT invent information. If something is unclear from the code, say "TBD" or omit the section.
10. Secrets are accessed via input substitution: `${secrets.secret_name}` — there is NO `getSecret()` method.
11. Module inputs use `getInput(inputs, "<input_name>")` — NOT indexed/numbered inputs.
12. Do NOT include a Table of Contents for short READMEs (under 100 lines).

## Template Structure

```markdown
# <Project Name>

<One-line description of what this project does.>

## Overview

<2-4 sentences explaining the purpose, who it's for, and where it fits in the Flomation platform.>

## Prerequisites

<What you need installed/configured before using this. Use a simple list.>

## Installation

<Step-by-step setup instructions. Be specific — include actual commands.>

## Configuration

<Table of environment variables, config files, or settings.>

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `EXAMPLE_VAR` | What it does | Yes/No | `value` |

## Inputs

<Only for Flomation modules. Table of module inputs.>

| Input | Type | Description | Required |
|-------|------|-------------|----------|
| `input_name` | string | What it does | Yes/No |

## Outputs

<Only for Flomation modules. Table of module outputs.>

| Output | Type | Description |
|--------|------|-------------|
| `result` | object | What it returns |

## Usage

<Real-world examples showing how to use this. Include code blocks with actual values (not "YOUR_VALUE_HERE" placeholders where avoidable).>

## Development

<How to run locally, run tests, build, lint — whatever applies.>

## CI/CD

<Only if a pipeline exists. Brief summary of what the pipeline does.>

## Project Structure

<Only for projects with more than 5 files. Brief tree showing key files.>

## Notes

<Any gotchas, known issues, or important context. Keep it brief.>

## Licence

<Licence type or "Proprietary — Flomation Ltd.">
```

## What to omit

- Omit any section that has no content (e.g., don't include "Inputs" for a Go service that has no module inputs).
- Omit "Project Structure" for small projects (under 5 files).
- Omit "CI/CD" if there's no pipeline config.
- Omit "Notes" if there's nothing noteworthy.
