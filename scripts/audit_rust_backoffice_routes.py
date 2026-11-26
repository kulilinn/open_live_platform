"""Count mounted Rust admin controller route registrations from an upstream checkout."""

import argparse
import re
from pathlib import Path


MERGE = re.compile(r"\.merge\s*\(\s*([a-z][a-z_0-9]*)::init_routes\b")
ROUTE = re.compile(r'\.route\s*\(\s*"([^"]+)"\s*,\s*([a-z_]\w*)\s*\(')


def uncomment(source):
    """Remove Rust comments, preserving string literals and source line numbers."""
    result = []
    position = 0
    while position < len(source):
        if source.startswith("//", position):
            end = source.find("\n", position)
            end = len(source) if end < 0 else end
            result.append(" " * (end - position))
            position = end
        elif source.startswith("/*", position):
            start, depth = position, 1
            position += 2
            while position < len(source) and depth:
                if source.startswith("/*", position):
                    depth += 1
                    position += 2
                elif source.startswith("*/", position):
                    depth -= 1
                    position += 2
                else:
                    position += 1
            result.append(re.sub(r"[^\n]", " ", source[start:position]))
        elif (raw := re.match(r'r(#{0,255})"', source[position:])) is not None:
            close = '"' + raw.group(1)
            end = source.find(close, position + len(raw.group(0)))
            end = len(source) if end < 0 else end + len(close)
            result.append(source[position:end])
            position = end
        elif source[position] == '"':
            start = position
            position += 1
            while position < len(source):
                char = source[position]
                position += 1
                if char == '"':
                    break
                if char == "\\":
                    position += 1
            result.append(source[start:position])
        else:
            result.append(source[position])
            position += 1
    return "".join(result)


def registrations(file):
    text = uncomment(file.read_text(encoding="utf-8"))
    return text, [
        (match.group(2).upper(), match.group(1), text.count("\n", 0, match.start()) + 1)
        for match in ROUTE.finditer(text)
    ]


def services(root, verbose):
    for crate in ("admin", "admin_mcn"):
        controllers = root / crate / "src" / "controllers"
        route_root = controllers / "mod.rs"
        text, root_routes = registrations(route_root)
        groups = [("controllers", route_root, root_routes)]
        for name in dict.fromkeys(MERGE.findall(text)):
            module = controllers / name / "mod.rs"
            if not module.exists():
                module = controllers / f"{name}.rs"
            _, routes = registrations(module)
            groups.append((name, module, routes))
        total = sum(len(routes) for _, _, routes in groups)
        print(f"{crate} /api controller registrations: {total}")
        for name, file, routes in groups:
            if not routes:
                continue
            print(f"  {name:24} {len(routes):3}  {file.relative_to(root)}")
            if verbose:
                for method, route, line in routes:
                    print(f"    {line:4} {method:5} {route}")
        main = root / crate / "src" / "main.rs"
        source = main.read_text(encoding="utf-8")
        count = len(ROUTE.findall(uncomment(source)))
        print(f"  main.rs direct registrations: {count}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("--verbose", action="store_true")
    args = parser.parse_args()
    services(args.source, args.verbose)
