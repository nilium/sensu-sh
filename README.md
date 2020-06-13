# sensu-sh

sensu-sh is intended for use with Sensu Go as an asset for running simple
bash-like scripts with access to event JSON without the need for temporary files
or echoing a variable into jq repeatedly to get different attributes of an
event.

This program is a mish-mash of the [sh][] and [gojq][] packages, using hooks in
the former and the query language of the latter to access information about
events with relative ease. sensu-sh, for the most part, is not something you
should actually use -- just write your scripts in Ruby or Python or write Go
programs so that your assets are easily executable. If you must write your
scripts in a bash-like shell scripting language, this is an option, I suppose.

Although I think this is useful and neat, sensu-sh should really be understood
as a hacky way to write shell scripts as Sensu Go handlers.

[sh]: https://mvdan.cc/sh
[gojq]: https://github.com/itchyny/gojq

Usage
---

**Usage:** `sensu-sh [options] <script-file|-> [-- args]`

It is not valid to pass `-` for both the script-file and event file. Only one
can be used with standard input. To accept piped event data from Sensu Go, the
default is to read the event from standard input and to execute a script file
from an asset.

**Options**

| Option            | Description
| -                 | -
| `-E, -event=FILE` | Set the file to read event data from. Defaults to `-` (standard input).
| `-R, -raw`        | Treat each argument as lines of script.
| `-- args`         | Pass additional arguments as positional arguments to the script.

The event data is parsed at startup. Failing to parse event data is a fatal
error.

### Command: event

To access event data, you can use the built-in `event` command, which takes
a single jq query string and a number of optional flags.

---

**Usage:** `event [options] [query]`

If no arguments are given, it is equivalent to running `event .`.

**Options:**

| Option          | Description
| -               | -
| `-j`, `-json`   | Print output as JSON.
| `-Y`, `-yaml`   | Print output as YAML.
| `-p`, `-pretty` | Pretty-print JSON output.

---

As an example, assuming an event arrived for an entity named `foobar`, you could
get the entity's name with the following script:

    #!sensu-sh
    echo $(event .entity.metadata.name)
    # Output: foobar


### Command: query

To query arbitrary JSON or YAML text using a jq query string, you can use the
built-in `query` command. This accepts JSON or YAML text from either standard
input (the default) or an environment variable. If the variable is indexed, each
element of the variable is queried separately.

---

**Usage:** `query [options] [query] [var|-]`

If no arguments are given, it is equivalent to running `query . -`, with it
parsing standard input and returning it.

**Options:**

| Option             | Description
| -                  | -
| `-R`, `-raw-input` | Do not decode the input and instead pass it directly to the query.
| `-j`, `-json`      | Print output as JSON.
| `-Y`, `-yaml`      | Print output as YAML.
| `-p`, `-pretty`    | Pretty-print JSON output.

---

The following is an example of using query to operate on variables:

    #!sensu-sh
    entity="$(event .entity)"
    meta="$(query .metadata entity)"
    echo "$(query .name meta)"

There is also shorthand for this, allowing you to query a variable by invoking
it as `@var`:

    #!sensu-sh
    entity="$(event .entity)"
    meta="$(@entity .metadata)"
    echo "$(@meta .name)"

This works as a hook on the sh interpreter's exec handler. Any invokation of
a command beginning with an `@` sign that matches an existing variable can be
queried this way.

Note that this applies to all variables, not just those extracted from an event.
For example, to split a colon-separated PATH variable, you could use `@PATH`:

    $ sensu-sh <<<{} -R "@PATH -R 'split(\":\")[]'"
    /usr/local/bin
    /usr/local/sbin
    /usr/bin
    /bin
    /usr/sbin
    /sbin

License
---

sensu-sh is licensed under a BSD 3-Clause license. It can be found in the
COPYING file.
