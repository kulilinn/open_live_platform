"""List LiveMedia's mounted client, service and backoffice routes and regenerate Go dispatch fixtures."""

import argparse
import re
from pathlib import Path




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


CALL = re.compile(r"\.(route|merge|nest)\s*\(")
LET = re.compile(r"\blet\s+(\w+)\s*=\s*([^;]+);")
VERB = re.compile(r"\b(get|post|delete|put|patch|head|options)\s*\(")


def arguments(text, offset):
    """Find the end of one call, skipping parentheses inside strings."""
    depth = 1
    for token in re.finditer(r'"(?:\\.|[^"\\])*"|[()]', text[offset:]):
        if token[0] == "(":
            depth += 1
        elif token[0] == ")":
            depth -= 1
            if depth == 0:
                end = offset + token.start()
                return text[offset:end].strip().rstrip(",").strip(), end + 1
    raise ValueError("unclosed router call")


def module_file(base, names):
    path = base.joinpath(*names)
    return path.with_suffix(".rs") if path.with_suffix(".rs").exists() else path / "mod.rs"


def router(file, prefix="", root=None, function="init_routes"):
    if root is None:
        root = file.parent.parent
    source = uncomment(file.read_text(encoding="utf-8-sig"))
    decl = re.search(r"\bfn\s+" + function + r"\s*\(", source)
    if not decl:
        export = re.search(r"pub use (\w+)::" + function + r";", source)
        if not export:
            raise ValueError(f"{file}: missing {function}")
        return router(module_file(file.parent, [export[1]]), prefix, root, function)
    body = source[decl.end():].split("\n}", 1)[0]
    variables = dict(LET.findall(body))

    def mounted(expr, mount):
        if expr in variables:
            yield from walk(variables[expr], mount)
        elif expr.startswith("Router::new()"):
            yield from walk(expr, mount)
        else:
            caller = re.match(r"((?:\w+::)+)(\w+)\(", expr)
            if not caller:
                raise ValueError(f"{file}: unknown router {expr}")
            parts = caller[1].rstrip(":").split("::")
            relative = file.parent
            if parts[0] == "crate":
                relative, parts = root, parts[1:]
            yield from router(module_file(relative, parts), mount, root, caller[2])

    def walk(expr, mount):
        offset = 0
        while match := CALL.search(expr, offset):
            arg, offset = arguments(expr, match.end())
            if match[1] == "merge":
                yield from mounted(arg, mount)
                continue
            path, target = re.fullmatch(r'"([^"]+)"\s*,\s*(.+)', arg, re.S).groups()
            if match[1] == "nest":
                yield from mounted(target.strip(), mount + path)
            else:
                verbs = VERB.findall(target)
                if not verbs:
                    raise ValueError(f"unknown route method in {file}: {target}")
                for method in verbs:
                    yield method.upper(), mount + path

    tail = LET.sub("", body)
    routes = list(walk(tail, prefix))
    # Debug and release branches may both return a previously built router.
    if not routes:
        for name, expr in variables.items():
            if re.search(r"\b" + name + r"\b", tail) and CALL.search(expr):
                routes.extend(walk(expr, prefix))
    return routes


def manifest(name, routes, verbose, fixtures):
    lines = sorted({f"{method} {path}" for method, path in routes})
    if len(lines) != len(routes):
        raise ValueError(f"{name}: duplicate mounted method/path")
    if verbose:
        print("\n".join(lines))
    if fixtures:
        path = fixtures / f"livemedia_{name}_routes.txt"
        with path.open("w", encoding="utf-8", newline="\n") as out:
            out.write("# Generated by scripts/audit_rust_backoffice_routes.py from the Rust checkout.\n")
            out.write("\n".join(lines) + "\n")


def services(root, verbose, fixtures):
    src = root / "api" / "src"
    routes = router(src / "route.rs", root=src, function="create_router")
    trusted = ("/api/auth_internal/", "/api/microservices/", "/schedule/")
    public = [r for r in routes if not r[1].startswith(trusted + ("/api/internal/",))]
    service = [r for r in routes if r[1].startswith(trusted)]
    print(f"client and callback routes: {len(public)}; trusted service and scheduler routes: {len(service)}")
    manifest("client", public, verbose, fixtures)
    manifest("service", service, verbose, fixtures)
    for crate in ("admin", "admin_mcn"):
        src = root / crate / "src"
        routes = router(src / "controllers" / "mod.rs")
        controller_paths = {path for method, path in routes}
        print(f"{crate}: {len(controller_paths)} /api paths, {len(routes)} method/path pairs")
        main = uncomment((src / "main.rs").read_text(encoding="utf-8-sig"))
        for path, method in re.findall(r'\.route\s*\(\s*"([^"]+)"\s*,\s*(\w+)\(', main):
            routes.append((method.upper(), path))
        manifest(crate, routes, verbose, fixtures)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("--verbose", action="store_true")
    parser.add_argument("--fixtures", type=Path, help="write route manifests to this directory")
    args = parser.parse_args()
    services(args.source, args.verbose, args.fixtures)
