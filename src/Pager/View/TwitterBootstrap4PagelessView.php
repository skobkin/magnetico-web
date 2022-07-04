<?php

declare(strict_types=1);

namespace App\Pager\View;

use Pagerfanta\Pagerfanta;
use Pagerfanta\PagerfantaInterface;
use Pagerfanta\View\Template\TemplateInterface;
use Pagerfanta\View\Template\TwitterBootstrap4Template;
use Pagerfanta\View\ViewInterface;

/**
 * Modified TwitterBootstrap4View.
 *
 * Removes page list and don't disable 'non-existing' pages.
 */
class TwitterBootstrap4PagelessView implements ViewInterface
{
    private TemplateInterface $template;

    /** @var Pagerfanta */
    private PagerfantaInterface $pagerfanta;

    private int $currentPage;

    public function __construct(TemplateInterface $template = null)
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
        return 'twitter_bootstrap4_pageless';
    }

    protected function createDefaultTemplate()
    {
        return new TwitterBootstrap4Template();
    }

    private function initializePagerfanta(PagerfantaInterface $pagerfanta)
    {
        $this->pagerfanta = $pagerfanta;
        $this->currentPage = $pagerfanta->getCurrentPage();
    }

    private function configureTemplate($routeGenerator, $options)
    {
        $this->template->setRouteGenerator($routeGenerator);
        $this->template->setOptions($options);
    }

    private function generate()
    {
        $pages = $this->generatePages();

        return $this->generateContainer($pages);
    }

    private function generateContainer($pages)
    {
        return str_replace('%pages%', $pages, $this->template->container());
    }

    private function generatePages()
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
