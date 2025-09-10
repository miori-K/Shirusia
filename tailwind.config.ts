import type { Config } from "tailwindcss"

const config: Config = {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",  // ← TypeScript対応で ts/tsx を忘れずに
  ],
  theme: {
    extend: {},
  },
  plugins: [],
}

export default config