<?php

declare(strict_types=1);

namespace App\Tests\Functional;

use App\Entity\User;
use Symfony\Bundle\FrameworkBundle\Test\KernelTestCase;
use Symfony\Component\HttpFoundation\Request;
use Symfony\Component\HttpFoundation\Session\Session;
use Symfony\Component\HttpFoundation\Session\Storage\MockArraySessionStorage;
use Symfony\Component\PasswordHasher\PasswordHasherInterface;
use Symfony\Component\Security\Core\Authentication\Token\UsernamePasswordToken;
use Twig\Environment;

class BaseTemplateRenderTest extends KernelTestCase
{
    public function testBaseTemplateRendersForAuthenticatedUser(): void
    {
        self::bootKernel();
        $container = static::getContainer();

        $hasher = $this->createStub(PasswordHasherInterface::class);
        $hasher->method('hash')->willReturn('hashed-password');
        $user = new User('test-user', $hasher, 'password', 'test@example.com', ['ROLE_USER']);
        $container->get('security.token_storage')->setToken(new UsernamePasswordToken($user, 'main', ['ROLE_USER']));

        $request = new Request();
        $request->attributes->set('_route', 'index');
        $request->setSession(new Session(new MockArraySessionStorage()));
        $container->get('request_stack')->push($request);

        $twig = $container->get('twig');
        self::assertInstanceOf(Environment::class, $twig);

        $template = $twig->createTemplate(
            (string) file_get_contents(\dirname(__DIR__, 2).'/templates/base.html.twig')
        );
        $html = $template->render([]);

        self::assertStringContainsString('autofocus', $html);
    }
}
