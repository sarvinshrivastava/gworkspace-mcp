/**
 * Prints the scrypt hash to store as vault DASHBOARD_ADMIN_PASSWORD_HASH.
 *
 *   npm run build && npm run hash-password
 *
 * The password is read from the terminal with echo off (or from stdin when
 * piped), never from argv — argv ends up in shell history and `ps`.
 */

import { hashPassword } from "./admin/security.js";
import { errorMessage } from "./errors.js";

function readHidden(prompt: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const stdin = process.stdin;
    process.stderr.write(prompt);
    stdin.setRawMode(true);
    stdin.resume();
    stdin.setEncoding("utf8");
    let value = "";
    const onData = (chunk: string) => {
      for (const ch of chunk) {
        if (ch === "\r" || ch === "\n") {
          stdin.setRawMode(false);
          stdin.pause();
          stdin.off("data", onData);
          process.stderr.write("\n");
          resolve(value);
          return;
        }
        if (ch === "\u0003") {
          stdin.setRawMode(false);
          reject(new Error("cancelled"));
          return;
        }
        if (ch === "\u007f" || ch === "\b") value = value.slice(0, -1);
        else value += ch;
      }
    };
    stdin.on("data", onData);
  });
}

async function readPiped(): Promise<string> {
  let data = "";
  for await (const chunk of process.stdin) data += String(chunk);
  return data.replace(/\r?\n$/, "");
}

async function main() {
  let password: string;
  if (process.stdin.isTTY) {
    password = await readHidden("Dashboard password: ");
    const again = await readHidden("Again: ");
    if (password !== again) throw new Error("the two entries differ");
  } else {
    password = await readPiped();
  }
  if (password.length < 12) throw new Error("use at least 12 characters");
  process.stdout.write(`${hashPassword(password)}\n`);
}

main().catch((error) => {
  console.error(errorMessage(error));
  process.exit(1);
});
