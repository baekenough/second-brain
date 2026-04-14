---
title: "Guide: Web Scraping"
type: guide
updated: 2026-04-12
sources:
  - guides/web-scraping/index.yaml
related:
  - "[[lang-python-expert]]"
  - "[[lang-typescript-expert]]"
  - "[[r001]]"
---

# Guide: Web Scraping

Reference documentation for web scraping — HTTP clients, HTML parsing, JavaScript rendering, rate limiting, and ethical scraping practices.

## Overview

The Web Scraping guide provides reference documentation for automated data collection from web sources. It covers both static content (requests + BeautifulSoup/cheerio) and dynamic JavaScript-rendered content (Playwright, Puppeteer), with emphasis on responsible scraping practices and rate limiting.

## Key Topics

- **HTTP Clients**: `requests`/`httpx` (Python), `fetch`/`axios` (Node.js), session management
- **HTML Parsing**: BeautifulSoup4 (Python), cheerio (Node.js), CSS selectors, XPath
- **JavaScript Rendering**: Playwright (Python/Node.js), Puppeteer, page wait strategies
- **Rate Limiting**: Respectful delays, robots.txt compliance, user-agent identification
- **Data Extraction**: Structured data (JSON-LD, microdata), regex patterns, table parsing
- **Error Handling**: Retry logic, timeout handling, selector fallbacks
- **Anti-Detection**: Header rotation, proxy usage, browser fingerprinting awareness

## Relationships

- **Python implementation**: [[lang-python-expert]] for Python scraping code
- **Node.js implementation**: [[lang-typescript-expert]] for TypeScript scraping
- **Safety**: [[r001]] — `WebFetch` requires approval; external data access rules apply

## Sources

- `guides/web-scraping/index.yaml`
