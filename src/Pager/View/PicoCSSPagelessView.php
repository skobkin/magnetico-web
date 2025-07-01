<?php
declare(strict_types=1);

namespace App\Pager\View;

use Pagerfanta\Pagerfanta;
use Pagerfanta\PagerfantaInterface;
use Pagerfanta\View\Template\TemplateInterface;
use Pagerfanta\View\ViewInterface;

/**
 * PicoCSSPagelessView: PicoCSS pagination styles.
 *
 * Removes page list and doesn't disable 'non-existing' pages.
 */
class PicoCSSPagelessView implements ViewInterface
{
    private TemplateInterface $template;

    /** @var Pagerfanta */
    private PagerfantaInterface $pagerfanta;

    private int $currentPage;

    public function __construct(?TemplateInterface $template = null)
    {
        $this->template = $template ?: $this->createDefaultTemplate();
    }

    public function render(PagerfantaInterface $pagerfanta, callable $routeGenerator, array $options = []): string
    {
        $this->initializePagerfanta($pagerfanta);

        $this->configureTemplate($routeGenerator, $options);

        return $this->generate();
    }

    public function getName(): string
    {
        return 'picocss_pageless';
    }

    protected function createDefaultTemplate()
    {
        return new class implements TemplateInterface {
            private $routeGenerator;
            private $options;
            public function setRouteGenerator(callable $routeGenerator): void { $this->routeGenerator = $routeGenerator; }
            public function setOptions(array $options): void { $this->options = $options; }
            public function container(): string { return '<nav class="pagination-nav"><ul class="pagination">%pages%</ul></nav>'; }
            public function page(int $page): string { return '<li><a href="' . call_user_func($this->routeGenerator, $page) . '" class="page-btn">' . $page . '</a></li>'; }
            public function pageWithText(int $page, string $text, ?string $rel = null): string {
                $relAttr = $rel ? ' rel="' . htmlspecialchars($rel) . '"' : '';
                return '<li><a href="' . call_user_func($this->routeGenerator, $page) . '" class="page-btn"' . $relAttr . '>' . htmlspecialchars($text) . '</a></li>';
            }
            public function previousDisabled(): string { return '<li class="disabled"><span class="page-btn">&laquo; Prev</span></li>'; }
            public function previousEnabled(int $page): string { return '<li><a href="' . call_user_func($this->routeGenerator, $page) . '" rel="prev" class="page-btn">&laquo; Prev</a></li>'; }
            public function nextDisabled(): string { return '<li class="disabled"><span class="page-btn">Next &raquo;</span></li>'; }
            public function nextEnabled(int $page): string { return '<li><a href="' . call_user_func($this->routeGenerator, $page) . '" rel="next" class="page-btn">Next &raquo;</a></li>'; }
            public function first(): string { return '<li><a href="' . call_user_func($this->routeGenerator, 1) . '" class="page-btn">&laquo; First</a></li>'; }
            public function last(int $page): string { return '<li><a href="' . call_user_func($this->routeGenerator, $page) . '" class="page-btn">Last &raquo;</a></li>'; }
            public function current(int $page): string { return '<li class="active"><span class="page-btn">' . $page . '</span></li>'; }
            public function separator(): string { return '<li class="disabled"><span class="page-btn">&hellip;</span></li>'; }
        };
    }

    private function initializePagerfanta(PagerfantaInterface $pagerfanta): void
    {
        $this->pagerfanta = $pagerfanta;
        $this->currentPage = $pagerfanta->getCurrentPage();
    }

    private function configureTemplate($routeGenerator, $options): void
    {
        $this->template->setRouteGenerator($routeGenerator);
        $this->template->setOptions($options);
    }

    private function generate(): array|string
    {
        $pages = $this->generatePages();

        return $this->generateContainer($pages);
    }

    private function generateContainer($pages): array|string
    {
        return str_replace('%pages%', $pages, $this->template->container());
    }

    private function generatePages(): string
    {
        return $this->previous().$this->currentPage().$this->next();
    }

    private function previous(): string
    {
        if ($this->currentPage > 1) {
            return $this->template->previousEnabled($this->pagerfanta->getPreviousPage());
        }

        return $this->template->previousDisabled();
    }

    private function next(): string
    {
        return $this->template->nextEnabled($this->currentPage + 1);
    }

    private function currentPage(): string
    {
        return $this->template->current($this->currentPage);
    }
}
