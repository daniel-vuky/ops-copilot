import js from "@eslint/js";
import { defineConfig, globalIgnores } from "eslint/config";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

// Text from alerts, tools and reports is rendered as text, never as HTML
// (ADR 0002 §6), and the CSP allows no inline styles.
const renderTextAsText = "Render text as text (ADR 0002 §6)";
const noInlineStyles = "Use a class from app.css; no inline styles (CSP)";

// The source guards, for every TypeScript file and for any plain JavaScript
// under src/, so a .js or .jsx file cannot slip past them.
const textAndStyleGuards = {
  "no-restricted-syntax": [
    "error",
    {
      selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']",
      message: renderTextAsText,
    },
    {
      selector: "Property[key.name='dangerouslySetInnerHTML']",
      message: renderTextAsText,
    },
    {
      selector: "Property[key.value='dangerouslySetInnerHTML']",
      message: renderTextAsText,
    },
    {
      selector: "JSXAttribute[name.name='style']",
      message: noInlineStyles,
    },
    {
      selector:
        "AssignmentExpression > MemberExpression.left[property.name=/^(innerHTML|outerHTML)$/]",
      message: renderTextAsText,
    },
    {
      selector:
        "AssignmentExpression > MemberExpression.left[computed=true][property.value=/^(innerHTML|outerHTML)$/]",
      message: renderTextAsText,
    },
    {
      selector:
        "CallExpression > MemberExpression.callee[property.name='insertAdjacentHTML']",
      message: renderTextAsText,
    },
    {
      selector:
        "CallExpression > MemberExpression.callee[object.name='document'][property.name=/^(write|writeln)$/]",
      message: renderTextAsText,
    },
    {
      selector: "JSXAttribute[name.name=/^srcdoc$/i]",
      message: renderTextAsText,
    },
    {
      selector:
        "CallExpression[callee.property.name='setAttribute'][arguments.0.value=/^style$/i]",
      message: noInlineStyles,
    },
  ],
};

export default defineConfig([
  globalIgnores(["dist"]),
  {
    files: ["**/*.{js,jsx,mjs,ts,tsx}"],
    extends: [js.configs.recommended],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
  },
  {
    files: ["scripts/**/*.mjs"],
    languageOptions: {
      globals: globals.node,
    },
  },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [tseslint.configs.recommended],
  },
  {
    files: ["**/*.{ts,tsx}", "src/**/*.{js,jsx,mjs}"],
    languageOptions: {
      parserOptions: { ecmaFeatures: { jsx: true } },
    },
    rules: textAndStyleGuards,
  },
  {
    files: ["**/*.{tsx,jsx}"],
    extends: [reactHooks.configs.flat.recommended],
  },
]);
