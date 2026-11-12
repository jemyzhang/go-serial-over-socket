# Terminal Font Family Feature

## Overview
Added a font family selector to the web terminal, allowing users to choose from popular monospace fonts specifically designed for coding.

## Changes Made

### 1. HTML Template (`web/templates/index.html`)
- **Added Google Fonts Link**: Imported popular coding fonts from Google Fonts in the `<head>` section
  - Fira Code
  - JetBrains Mono
  - Source Code Pro
  - Roboto Mono
  - Inconsolata
  - Ubuntu Mono

- **Added Font Family Selector**: New dropdown in the Terminal Settings panel with the following options:
  - **Fira Code** - Popular font with ligatures for programming
  - **JetBrains Mono** - Designed by JetBrains for developers
  - **Source Code Pro** - Adobe's monospace font for coding
  - **Cascadia Code** - Microsoft's modern coding font
  - **Consolas** - Classic Microsoft monospace font
  - **Monaco** - Default macOS terminal font (selected by default)
  - **Menlo** - macOS system monospace font
  - **Ubuntu Mono** - Ubuntu's monospace font
  - **Courier New** - Classic monospace font
  - **Roboto Mono** - Google's monospace font
  - **Inconsolata** - Popular open-source coding font

### 2. JavaScript (`web/static/js/app.js`)
- **Updated `initTerminal()` function**:
  - Loads saved font family from localStorage
  - Sets the dropdown to the saved value
  - Applies the font family to the terminal on initialization

- **Updated `updateTerminalSettings()` function**:
  - Reads the selected font family from the dropdown
  - Applies it to the terminal
  - Saves the selection to localStorage for persistence

## Usage
1. Open the web terminal
2. Navigate to the **Terminal Settings** panel in the left sidebar
3. Find the **Font Family** dropdown (below the Theme selector)
4. Select your preferred coding font
5. The font will be applied immediately and saved for future sessions

## Font Fallbacks
Each font option includes fallback fonts to ensure compatibility:
- Primary font (e.g., 'Fira Code')
- Monaco (macOS fallback)
- Menlo (macOS fallback)
- monospace (system fallback)

This ensures the terminal always displays with a monospace font even if the selected font isn't available on the system.

## Persistence
The selected font family is saved to the browser's localStorage, so your preference will be remembered across sessions.

## Browser Compatibility
- The fonts are loaded from Google Fonts CDN
- Preconnect links optimize font loading performance
- All fonts are widely supported across modern browsers
