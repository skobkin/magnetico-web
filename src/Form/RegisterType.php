<?php

declare(strict_types=1);

namespace App\Form;

use App\Form\Data\RegisterData;
use Karser\Recaptcha3Bundle\Form\Recaptcha3Type;
use Karser\Recaptcha3Bundle\Validator\Constraints\Recaptcha3;
use Symfony\Component\Form\AbstractType;
use Symfony\Component\Form\Extension\Core\Type\EmailType;
use Symfony\Component\Form\Extension\Core\Type\PasswordType;
use Symfony\Component\Form\Extension\Core\Type\TextType;
use Symfony\Component\Form\FormBuilderInterface;
use Symfony\Component\OptionsResolver\OptionsResolver;

class RegisterType extends AbstractType
{
    public function buildForm(FormBuilderInterface $builder, array $options): void
    {
        $builder
            ->add('username', TextType::class, ['required' => true])
            ->add('password', PasswordType::class, ['required' => true])
            ->add('email', EmailType::class, ['required' => true])
            ->add('inviteCode', TextType::class, ['required' => true])
            ->add('recaptcha', Recaptcha3Type::class, [
                'action_name' => 'register',
                'constraints' => new Recaptcha3(),
            ])
        ;
    }

    public function configureOptions(OptionsResolver $resolver): void
    {
        $resolver->setDefaults([
            'data_class' => RegisterData::class,
        ]);
    }
}
