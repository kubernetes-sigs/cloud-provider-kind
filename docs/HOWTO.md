# Documentation Guide

This documentation site is generated and served using **Docsify**, a lightweight documentation site generator that renders Markdown files on the fly.

## Serving the Docsify Site

To run the documentation locally:

```bash
docsify serve ./docs
```

This command starts a local server and serves the documentation from the current directory.
Open the provided URL in your browser to view the site.

---

## Adding a New Link to the Sidebar

The sidebar content is managed by the `_sidebar.md` file.
To add a new link:

1. Open `_sidebar.md`
2. Insert a new list item pointing to your Markdown file, for example:

```markdown
- [Install Docker](user/install/install_docker.md)
- [Gateway API Overview](user/gateway/gatewayapi.md)
```

Docsify will automatically load the linked page when clicked.

---

## Adding a New Page

To create a new documentation page:

1. Add a new `.md` file under the appropriate folder.
   Example:

```
user/example/new_feature_guide.md
```

2. Add a link to this file in `_sidebar.md` so it appears in navigation.

---

## Folder Structure Overview

Below is the current folder hierarchy used for organizing the docs:

```
├── code-of-conduct.md               # Code of Conduct guidelines
├── contributing/
│   └── CONTRIBUTING.md              # Contribution guide
├── _coverpage.md                    # Docsify cover page configuration
├── design/
│   └── images                       # Design-related assets
├── HOWTO.md                         # General how-to guide
├── index.html                       # Docsify entry point
├── README.md                        # Project introduction
├── _sidebar.md                      # Sidebar navigation config
└── user/
    ├── example/                     # Example implementation guides
    ├── gateway/                     # Gateway-related configuration guides
    ├── howto.md                     # Additional user how-tos
    ├── images/                      # Image assets for user docs
    ├── ingress/                     # Ingress-related guides
    ├── install/                     # Installation instructions
    ├── os_support.md                # OS support documentation
    └── support/                     # Support-related docs
```

This structure helps keep the content organized by feature areas and documentation types.

---

## Further Configuration

For more advanced configuration, theming, plugins, and deployment options, please refer to the official Docsify documentation:

[https://docsify.js.org](https://docsify.js.org)

