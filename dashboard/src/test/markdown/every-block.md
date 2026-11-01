# Every block the editor writes

A paragraph with **strong**, __strong by underscores__, *emphasis*, _emphasis by
underscores_, ~~struck~~, `inline code`, a [link](https://example.com/docs "The docs"),
an [in-app link](#/doclet/pages/7c1e), an autolink <https://example.com>, and a
picture ![a chart](images/chart.png "Quarterly").

## Lists

- one
- two
  - nested under two
  - and its sibling
- three

* a star bullet
* another

1. first
2. second
3. third

7) starts at seven
8) and goes on

- [ ] a task to do
- [x] a task done
  - [ ] a nested task

- loose item one

- loose item two

## Quotes and callouts

> A quote that runs
> over two lines.
>
> And a second paragraph.

> [!NOTE]
> A note for the reader.

> [!TIP]
> A tip with **emphasis**.

> [!IMPORTANT]
> Something important.

> [!WARNING]
> A warning.

> [!CAUTION]
> A caution, with a list:
>
> - first risk
> - second risk

## Code

```go
func main() {
	fmt.Println("hello")
}
```

~~~
plain text in tildes, holding a ``` fence line
```
~~~

```sh
# a comment that is not a heading
echo "- not a list"
```

## Tables

| Backend | CAS | Ran a second estate |
|---|:---:|---:|
| Pulsar | no | always |
| NATS | yes | never |
| A \| pipe | `code` | **bold** |

## Rules and breaks

---

***

___

A line ending in two spaces breaks hard,  
like this, and a backslash breaks too\
like this.

### A heading with closing hashes ###

#### Level four

##### Level five

###### Level six
Text before any heading, with snake_case_names and a literal \*star\* and a \# hash.

Raw <b>tags</b> and an & ampersand stay text, as do <script>alert(1)</script> and [refused](javascript:alert(1)).

A [link with \] bracket](#/doclet/pages/42) and a [paren link](https://en.wikipedia.org/wiki/Markdown_(disambiguation)).

Title over a rule
---

   Indented paragraph text
that continues.

- item with a paragraph

  and a second paragraph under it
- item after

- [x] done
- plain item in a task list

10. numbered from ten
11. eleven

```
unclosed fence runs to the end
